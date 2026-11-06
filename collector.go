package main

import (
	"bufio"
	"cmp"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	historySeconds = 1800
	topProcs       = 15
)

var (
	skipDisks = []string{"loop", "ram", "zram", "dm-", "md", "sr", "fd", "nbd"}
	skipFS    = map[string]bool{"tmpfs": true, "devtmpfs": true, "overlay": true, "shm": true, "udev": true, "none": true}
)

const remoteScript = `
echo @@info
hostname
uname -sr
grep -c ^processor /proc/cpuinfo
getconf CLK_TCK
getconf PAGESIZE
echo @@nics
for i in /sys/class/net/*; do [ -e "$i/device" ] && echo "${i##*/}"; done
echo @@disks
ls /sys/block
while :; do
  echo @@sample
  echo @@uptime; cat /proc/uptime
  echo @@stat; head -n 1 /proc/stat
  echo @@loadavg; cat /proc/loadavg
  echo @@meminfo; cat /proc/meminfo
  echo @@netdev; cat /proc/net/dev
  echo @@diskstats; cat /proc/diskstats
  echo @@df; df -Pkl 2>/dev/null
  echo @@procstat; cat /proc/[0-9]*/stat 2>/dev/null
  echo @@ps; ps -eo pid=,user:32=,args= 2>/dev/null | cut -c1-256
  echo @@end
  sleep INTERVAL
done
`

type Info struct {
	Hostname string   `json:"hostname"`
	Kernel   string   `json:"kernel"`
	CPUs     int      `json:"cpus"`
	ClkTck   uint64   `json:"-"`
	PageSize uint64   `json:"-"`
	NICs     []string `json:"-"`
	Disks    []string `json:"-"`
}

type FS struct {
	Mount  string  `json:"mount"`
	Device string  `json:"device"`
	Size   uint64  `json:"size"`
	Used   uint64  `json:"used"`
	Avail  uint64  `json:"avail"`
	Pct    float64 `json:"pct"`
}

type Proc struct {
	PID     int     `json:"pid"`
	User    string  `json:"user"`
	Command string  `json:"command"`
	CPU     float64 `json:"cpu"`
	RSS     uint64  `json:"rss"`
	Mem     float64 `json:"mem"`
}

type Latest struct {
	Uptime    float64  `json:"uptime"`
	MemTotal  uint64   `json:"mem_total"`
	SwapTotal uint64   `json:"swap_total"`
	FS        []FS     `json:"fs"`
	NICs      []string `json:"nics"`
	Disks     []string `json:"disks"`
	ProcCount int      `json:"proc_count"`
	Procs     []Proc   `json:"procs"`
}

type Snapshot struct {
	Host     string               `json:"host"`
	Interval int                  `json:"interval"`
	Status   string               `json:"status"`
	Info     Info                 `json:"info"`
	Latest   Latest               `json:"latest"`
	Series   map[string][]float64 `json:"series"`
}

type counters struct {
	uptime float64
	cpu    [8]uint64
	nics   map[string][2]uint64 // rx, tx bytes
	disks  map[string][3]uint64 // read, written bytes; io_ticks ms
	procs  map[int]uint64       // utime+stime ticks
}

type Collector struct {
	host     string
	interval int

	mu      sync.Mutex
	status  string
	info    Info
	latest  Latest
	samples []map[string]float64

	prev *counters
}

func NewCollector(host string, interval int) *Collector {
	return &Collector{host: host, interval: interval, status: "connecting"}
}

func (c *Collector) Run() {
	script := strings.ReplaceAll(remoteScript, "INTERVAL", strconv.Itoa(c.interval))
	for {
		c.setStatus("connecting")
		cmd := exec.Command("ssh", "-T",
			"-o", "ServerAliveInterval=10",
			"-o", "ServerAliveCountMax=3",
			c.host, "sh -c '"+script+"'")
		cmd.Stderr = os.Stderr
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			log.Print(err)
		} else {
			c.read(out)
			cmd.Process.Kill()
			cmd.Wait()
		}
		c.prev = nil
		c.setStatus("disconnected, reconnecting")
		time.Sleep(5 * time.Second)
	}
}

func (c *Collector) setStatus(s string) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
}

func (c *Collector) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	block := map[string][]string{}
	section := ""
	for sc.Scan() {
		line := sc.Text()
		tag, isTag := strings.CutPrefix(line, "@@")
		if !isTag {
			if section != "" {
				block[section] = append(block[section], line)
			}
			continue
		}
		switch tag {
		case "sample":
			if _, ok := block["info"]; ok {
				c.setInfo(block)
			}
			block, section = map[string][]string{}, ""
		case "end":
			c.add(block)
			block, section = map[string][]string{}, ""
		default:
			section = tag
			block[section] = nil
		}
	}
}

func (c *Collector) setInfo(b map[string][]string) {
	info := Info{NICs: b["nics"], ClkTck: 100, PageSize: 4096}
	if l := b["info"]; len(l) >= 5 {
		info.Hostname, info.Kernel = l[0], l[1]
		info.CPUs, _ = strconv.Atoi(l[2])
		if v, err := strconv.ParseUint(l[3], 10, 64); err == nil && v > 0 {
			info.ClkTck = v
		}
		if v, err := strconv.ParseUint(l[4], 10, 64); err == nil && v > 0 {
			info.PageSize = v
		}
	}
	for _, d := range b["disks"] {
		if !slices.ContainsFunc(skipDisks, func(p string) bool { return strings.HasPrefix(d, p) }) {
			info.Disks = append(info.Disks, d)
		}
	}
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
}

func fieldsAt(lines []string, i int) []string {
	if i < len(lines) {
		return strings.Fields(lines[i])
	}
	return nil
}

func u64(f []string, i int) uint64 {
	if i < len(f) {
		v, _ := strconv.ParseUint(f[i], 10, 64)
		return v
	}
	return 0
}

func f64(f []string, i int) float64 {
	if i < len(f) {
		v, _ := strconv.ParseFloat(f[i], 64)
		return v
	}
	return 0
}

func delta(a, b uint64) float64 {
	if a < b {
		return 0
	}
	return float64(a - b)
}

func (c *Collector) add(b map[string][]string) {
	c.mu.Lock()
	info := c.info
	c.mu.Unlock()

	cur := &counters{
		uptime: f64(fieldsAt(b["uptime"], 0), 0),
		nics:   map[string][2]uint64{},
		disks:  map[string][3]uint64{},
		procs:  map[int]uint64{},
	}
	if cur.uptime == 0 {
		log.Print("bad sample: no /proc/uptime")
		return
	}

	stat := fieldsAt(b["stat"], 0)
	for i := range cur.cpu {
		cur.cpu[i] = u64(stat, i+1)
	}
	load := fieldsAt(b["loadavg"], 0)

	mem := map[string]uint64{}
	for _, line := range b["meminfo"] {
		if k, v, ok := strings.Cut(line, ":"); ok {
			mem[k] = u64(strings.Fields(v), 0) * 1024
		}
	}
	memAvailable, ok := mem["MemAvailable"]
	if !ok {
		memAvailable = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
	}

	for i, line := range b["netdev"] {
		name, data, ok := strings.Cut(line, ":")
		if i < 2 || !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if slices.Contains(info.NICs, name) || (len(info.NICs) == 0 && name != "lo") {
			f := strings.Fields(data)
			cur.nics[name] = [2]uint64{u64(f, 0), u64(f, 8)}
		}
	}

	for _, line := range b["diskstats"] {
		f := strings.Fields(line)
		if len(f) > 12 && slices.Contains(info.Disks, f[2]) {
			cur.disks[f[2]] = [3]uint64{u64(f, 5) * 512, u64(f, 9) * 512, u64(f, 12)}
		}
	}

	var fs []FS
	for i, line := range b["df"] {
		f := strings.Fields(line)
		if i == 0 || len(f) < 6 || skipFS[f[0]] {
			continue
		}
		e := FS{
			Mount:  strings.Join(f[5:], " "),
			Device: f[0],
			Size:   u64(f, 1) * 1024,
			Used:   u64(f, 2) * 1024,
			Avail:  u64(f, 3) * 1024,
		}
		if e.Used+e.Avail > 0 {
			e.Pct = float64(e.Used) / float64(e.Used+e.Avail) * 100
		}
		fs = append(fs, e)
	}

	procs := map[int]*Proc{}
	for _, line := range b["procstat"] {
		open, end := strings.IndexByte(line, '('), strings.LastIndexByte(line, ')')
		if open < 0 || end < open {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
		f := strings.Fields(line[end+1:]) // f[0] is field 3 of /proc/pid/stat
		if err != nil || len(f) < 22 {
			continue
		}
		cur.procs[pid] = u64(f, 11) + u64(f, 12)
		procs[pid] = &Proc{PID: pid, Command: line[open+1 : end], RSS: u64(f, 21) * info.PageSize}
	}
	for _, line := range b["ps"] {
		pid, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		user, args, _ := strings.Cut(strings.TrimSpace(rest), " ")
		n, _ := strconv.Atoi(pid)
		if p := procs[n]; p != nil {
			p.User = user
			if args = strings.TrimSpace(args); args != "" {
				p.Command = args
			}
		}
	}

	latest := Latest{
		Uptime:    cur.uptime,
		MemTotal:  mem["MemTotal"],
		SwapTotal: mem["SwapTotal"],
		FS:        fs,
		NICs:      slices.Sorted(maps.Keys(cur.nics)),
		Disks:     slices.Sorted(maps.Keys(cur.disks)),
		ProcCount: len(procs),
	}
	prev := c.prev
	c.prev = cur

	if prev == nil || cur.uptime <= prev.uptime {
		c.mu.Lock()
		c.latest = latest
		c.mu.Unlock()
		return
	}

	dt := cur.uptime - prev.uptime
	var d [8]float64
	total := 0.0
	for i := range d {
		d[i] = delta(cur.cpu[i], prev.cpu[i])
		total += d[i]
	}
	if total == 0 {
		total = 1
	}

	s := map[string]float64{
		"t":          float64(time.Now().UnixMilli()) / 1000,
		"cpu_user":   (d[0] + d[1]) / total * 100,
		"cpu_system": (d[2] + d[5] + d[6]) / total * 100,
		"cpu_iowait": d[4] / total * 100,
		"cpu_steal":  d[7] / total * 100,
		"mem_used":   float64(mem["MemTotal"] - min(memAvailable, mem["MemTotal"])),
		"mem_cache":  float64(mem["Buffers"] + mem["Cached"] + mem["SReclaimable"]),
		"swap_used":  float64(mem["SwapTotal"] - min(mem["SwapFree"], mem["SwapTotal"])),
		"load1":      f64(load, 0),
		"load5":      f64(load, 1),
		"load15":     f64(load, 2),
	}
	for name, v := range cur.nics {
		p := prev.nics[name]
		rx, tx := delta(v[0], p[0])/dt, delta(v[1], p[1])/dt
		s["net_rx:"+name], s["net_tx:"+name] = rx, tx
		s["net_rx"] += rx
		s["net_tx"] += tx
	}
	for name, v := range cur.disks {
		p := prev.disks[name]
		rd, wr := delta(v[0], p[0])/dt, delta(v[1], p[1])/dt
		s["disk_read:"+name], s["disk_write:"+name] = rd, wr
		s["disk_util:"+name] = min(100, delta(v[2], p[2])/(dt*1000)*100)
		s["disk_read"] += rd
		s["disk_write"] += wr
	}

	list := make([]Proc, 0, len(procs))
	for pid, p := range procs {
		if t, ok := prev.procs[pid]; ok {
			p.CPU = delta(cur.procs[pid], t) / float64(info.ClkTck) / dt * 100
		}
		if latest.MemTotal > 0 {
			p.Mem = float64(p.RSS) / float64(latest.MemTotal) * 100
		}
		list = append(list, *p)
	}
	seen := map[int]bool{}
	for _, by := range []func(a, b Proc) int{
		func(a, b Proc) int { return cmp.Compare(b.CPU, a.CPU) },
		func(a, b Proc) int { return cmp.Compare(b.RSS, a.RSS) },
	} {
		slices.SortFunc(list, by)
		for _, p := range list[:min(topProcs, len(list))] {
			if !seen[p.PID] {
				seen[p.PID] = true
				latest.Procs = append(latest.Procs, p)
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.latest = latest
	c.samples = append(c.samples, s)
	if n := historySeconds / c.interval; len(c.samples) > n {
		c.samples = c.samples[len(c.samples)-n:]
	}
	c.status = "connected"
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	series := map[string][]float64{}
	if n := len(c.samples); n > 0 {
		for k := range c.samples[n-1] {
			vals := make([]float64, n)
			for i, s := range c.samples {
				vals[i] = s[k]
			}
			series[k] = vals
		}
	}
	return Snapshot{
		Host:     c.host,
		Interval: c.interval,
		Status:   c.status,
		Info:     c.info,
		Latest:   c.latest,
		Series:   series,
	}
}
