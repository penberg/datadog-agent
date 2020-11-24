// +build linux

package procutil

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	// ClockTicks is the number of clock ticks per second
	// C.sysconf(C._SC_CLK_TCK)
	ClockTicks = 100
	// WorldReadable represents file permission that's world readable
	WorldReadable os.FileMode = 4
)

type statusInfo struct {
	name       string
	status     string
	uids       []int32
	gids       []int32
	nspid      int32
	numThreads int32

	memInfo     *MemoryInfoStat
	ctxSwitches *NumCtxSwitchesStat
}

type statInfo struct {
	ppid       int32
	createTime int64
	nice       int32

	cpuStat *CPUTimesStat
}

// Probe is a service that fetches process related info on current host
type Probe struct {
	procRootLoc  string // ProcFS
	procRootFile *os.File

	// uid and euid are cached to minimize system call when check file permission
	uid  uint32 // UID
	euid uint32 // Effective UID

	bootTime uint64
}

// NewProcessProbe initializes a new Probe object
func NewProcessProbe() *Probe {
	hostProc := util.HostProc()
	bootTime, _ := bootTime(hostProc)

	p := &Probe{
		procRootLoc: hostProc,
		uid:         uint32(os.Getuid()),
		euid:        uint32(os.Geteuid()),
		bootTime:    bootTime,
	}
	return p
}

// Close cleans up everything related to Probe object
func (p *Probe) Close() {
	if p.procRootFile != nil {
		p.procRootFile.Close()
		p.procRootFile = nil
	}
}

// ProcessesByPID returns a map of process info indexed by PID
func (p *Probe) ProcessesByPID(now time.Time) (map[int32]*Process, error) {
	pids, err := p.getActivePIDs()
	if err != nil {
		return nil, err
	}

	procsByPID := make(map[int32]*Process, len(pids))
	for _, pid := range pids {
		pathForPID := filepath.Join(p.procRootLoc, strconv.Itoa(int(pid)))
		if !util.PathExists(pathForPID) {
			log.Debugf("Unable to create new process %d, dir doesn't exist", pid)
			continue
		}

		cmdline := p.getCmdline(pathForPID)
		if len(cmdline) == 0 {
			// NOTE: The agent's process check currently skips all processes that have no cmdline (i.e kernel processes).
			//       Moving this check down the stack saves us from a number of needless follow-up system calls.
			//       In the test resources for Postgres, this accounts for ~30% of processes.
			continue
		}

		statusInfo := p.parseStatus(pathForPID)
		ioInfo := p.parseIO(pathForPID)
		statInfo := p.parseStat(pathForPID, pid, now)

		procsByPID[pid] = &Process{
			Pid:     pid,               // /proc/{pid}
			Ppid:    statInfo.ppid,     // /proc/{pid}/{stat}
			Cmdline: cmdline,           // /proc/{pid}/cmdline
			Name:    statusInfo.name,   // /proc/{pid}/status
			Status:  statusInfo.status, // /proc/{pid}/status
			Uids:    statusInfo.uids,   // /proc/{pid}/status
			Gids:    statusInfo.gids,   // /proc/{pid}/status
			NsPid:   statusInfo.nspid,  // /proc/{pid}/status
			Stats: &Stats{
				CreateTime:  statInfo.createTime,    // /proc/{pid}/{stat}
				Nice:        statInfo.nice,          // /proc/{pid}/{stat}
				CPUTime:     statInfo.cpuStat,       // /proc/{pid}/{stat}
				MemInfo:     statusInfo.memInfo,     // /proc/{pid}/status or statm
				CtxSwitches: statusInfo.ctxSwitches, // /proc/{pid}/status
				NumThreads:  statusInfo.numThreads,  // /proc/{pid}/status
				IOStat:      ioInfo,                 // /proc/{pid}/io, requires permission checks
			},
		}
	}

	return procsByPID, nil
}

func (p *Probe) getRootProcFile() (*os.File, error) {
	if p.procRootFile != nil { // TODO (sk): Should we consider refreshing the file descriptor occasionally?
		return p.procRootFile, nil
	}

	f, err := os.Open(p.procRootLoc)
	if err == nil {
		p.procRootFile = f
	}

	return f, err
}

// getActivePIDs retrieves a list of PIDs representing actively running processes.
func (p *Probe) getActivePIDs() ([]int32, error) {
	procFile, err := p.getRootProcFile()
	if err != nil {
		return nil, err
	}

	fnames, err := procFile.Readdirnames(-1)
	if err != nil {
		return nil, err
	}

	// reset read offset to 0, so next time we could read the whole directory again
	_, err = procFile.Seek(0, 0)
	if err != nil {
		return nil, err
	}

	pids := make([]int32, 0, len(fnames))
	for _, fname := range fnames {
		pid, err := strconv.ParseInt(fname, 10, 32)
		if err != nil { // if not numeric name, just skip
			continue
		}
		pids = append(pids, int32(pid))
	}

	return pids, nil
}

// getCmdline retrieves the command line text from "cmdline" file for a process in procfs
func (p *Probe) getCmdline(pidPath string) []string {
	cmdline, err := ioutil.ReadFile(filepath.Join(pidPath, "cmdline"))
	if err != nil {
		log.Debugf("Unable to read process command line from %s: %s", pidPath, err)
		return nil
	}

	if len(cmdline) == 0 {
		return nil
	}

	return trimAndSplitBytes(cmdline)
}

// parseStatus retrieves io info from "io" file for a process in procfs
func (p *Probe) parseIO(pidPath string) *IOCountersStat {
	path := filepath.Join(pidPath, "io")
	var err error

	io := &IOCountersStat{}

	if err = p.ensurePathReadable(path); err != nil {
		return io
	}

	content, err := ioutil.ReadFile(path)
	if err != nil {
		return io
	}

	index := 0
	for i, r := range content {
		if r == '\n' {
			p.parseIOLine(content[index:i], io)
			index = i + 1
		}
	}

	return io
}

// parseIOLine extracts key and value for each line in "io" file
func (p *Probe) parseIOLine(line []byte, io *IOCountersStat) {
	for i := range line {
		if i+2 < len(line) && line[i] == ':' && line[i+1] == ' ' {
			key := line[0:i]
			value := line[i+2:]
			p.parseIOKV(string(key), string(value), io)
			break
		}
	}
}

// parseIOKV matches key with a field in IOCountersStat model and fills in the value
func (p *Probe) parseIOKV(key, value string, io *IOCountersStat) {
	switch key {
	case "syscr":
		v, err := strconv.ParseUint(value, 10, 64)
		if err == nil {
			io.ReadCount = v
		}
	case "syscw":
		v, err := strconv.ParseUint(value, 10, 64)
		if err == nil {
			io.WriteCount = v
		}
	case "read_bytes":
		v, err := strconv.ParseUint(value, 10, 64)
		if err == nil {
			io.ReadBytes = v
		}
	case "write_bytes":
		v, err := strconv.ParseUint(value, 10, 64)
		if err == nil {
			io.WriteBytes = v
		}
	}
}

// parseStatus retrieves status info from "status" file for a process in procfs
func (p *Probe) parseStatus(pidPath string) *statusInfo {
	path := filepath.Join(pidPath, "status")
	var err error

	sInfo := &statusInfo{
		uids:        make([]int32, 0),
		gids:        make([]int32, 0),
		memInfo:     &MemoryInfoStat{},
		ctxSwitches: &NumCtxSwitchesStat{},
	}

	content, err := ioutil.ReadFile(path)
	if err != nil {
		return sInfo
	}

	index := 0
	for i, r := range content {
		if r == '\n' {
			p.parseStatusLine(content[index:i], sInfo)
			index = i + 1
		}
	}

	return sInfo
}

// parseStatusLine takes each line in "status" file and parses info from it
func (p *Probe) parseStatusLine(line []byte, sInfo *statusInfo) {
	for i := range line {
		if i+2 < len(line) && line[i] == ':' && line[i+1] == '\t' {
			key := line[0:i]
			value := line[i+2:]
			p.parseStatusKV(string(key), string(value), sInfo)
			break
		}
	}
}

// parseStatusKV takes tokens parsed from each line in "status" file and populates statusInfo object
func (p *Probe) parseStatusKV(key, value string, sInfo *statusInfo) {
	switch key {
	case "Name":
		sInfo.name = strings.Trim(value, " \t")
	case "State":
		sInfo.status = value[0:1]
	case "Uid":
		sInfo.uids = make([]int32, 0, 4)
		for _, i := range strings.Split(value, "\t") {
			v, err := strconv.ParseInt(i, 10, 32)
			if err == nil {
				sInfo.uids = append(sInfo.uids, int32(v))
			}
		}
	case "Gid":
		sInfo.gids = make([]int32, 0, 4)
		for _, i := range strings.Split(value, "\t") {
			v, err := strconv.ParseInt(i, 10, 32)
			if err == nil {
				sInfo.gids = append(sInfo.gids, int32(v))
			}
		}
	case "NSpid":
		v, err := strconv.ParseInt(value, 10, 32)
		if err == nil {
			sInfo.nspid = int32(v)
		}
	case "Threads":
		v, err := strconv.ParseInt(value, 10, 32)
		if err == nil {
			sInfo.numThreads = int32(v)
		}
	case "voluntary_ctxt_switches":
		v, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			sInfo.ctxSwitches.Voluntary = v
		}
	case "nonvoluntary_ctxt_switches":
		v, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			sInfo.ctxSwitches.Involuntary = v
		}
	case "VmRSS":
		value := strings.Trim(value, " kB") // trim spaces and suffix "kB"
		v, err := strconv.ParseUint(value, 10, 64)
		if err == nil {
			sInfo.memInfo.RSS = v * 1024
		}
	case "VmSize":
		value := strings.Trim(value, " kB") // trim spaces and suffix "kB"
		v, err := strconv.ParseUint(value, 10, 64)
		if err == nil {
			sInfo.memInfo.VMS = v * 1024
		}
	case "VmSwap":
		value := strings.Trim(value, " kB") // trim spaces and suffix "kB"
		v, err := strconv.ParseUint(value, 10, 64)
		if err == nil {
			sInfo.memInfo.Swap = v * 1024
		}
	}
}

// parseStat retrieves stat info from "stat" file for a process in procfs
func (p *Probe) parseStat(pidPath string, pid int32, now time.Time) *statInfo {
	path := filepath.Join(pidPath, "stat")
	var err error

	sInfo := &statInfo{
		cpuStat: &CPUTimesStat{},
	}

	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return sInfo
	}

	sInfo = p.parseStatContent(contents, sInfo, pid, now)
	return sInfo
}

// parseStatContent takes the content of "stat" file and parses the values we care about
func (p *Probe) parseStatContent(statContent []byte, sInfo *statInfo, pid int32, now time.Time) *statInfo {
	// We want to skip past the executable name, which is wrapped in one or more parenthesis
	startIndex := bytes.LastIndexByte(statContent, byte(')'))
	if startIndex == -1 || startIndex+1 >= len(statContent) {
		return sInfo
	}

	content := statContent[startIndex+1:]
	// use spaces and prevChartIsSpace to simulate strings.Fields() to avoid allocation
	spaces := 0
	prevCharIsSpace := false
	var ppidStr, utimeStr, stimeStr, startTimeStr string

	for i := range content {
		if unicode.IsSpace(rune(content[i])) {
			if !prevCharIsSpace {
				spaces++
			}
			prevCharIsSpace = true
			continue
		} else {
			prevCharIsSpace = false
		}

		if spaces == 2 {
			ppidStr += string(content[i])
		} else if spaces == 12 {
			utimeStr += string(content[i])
		} else if spaces == 13 {
			stimeStr += string(content[i])
		} else if spaces == 20 {
			startTimeStr += string(content[i])
		}
	}

	if spaces <= 20 { // We access index 20 and below, so this is just a safety check.
		return sInfo
	}

	ppid, err := strconv.ParseInt(ppidStr, 10, 32)
	if err == nil {
		sInfo.ppid = int32(ppid)
	}

	utime, err := strconv.ParseFloat(utimeStr, 64)
	if err == nil {
		sInfo.cpuStat.User = utime / ClockTicks
	}
	stime, err := strconv.ParseFloat(stimeStr, 64)
	if err == nil {
		sInfo.cpuStat.System = stime / ClockTicks
	}
	// the nice parameter location seems to be different for various procfs,
	// so we fetch that using syscall
	snice, err := syscall.Getpriority(syscall.PRIO_PROCESS, int(pid))
	if err == nil {
		sInfo.nice = int32(snice)
	}

	sInfo.cpuStat.CPU = "cpu"
	sInfo.cpuStat.Timestamp = now.Unix()

	t, err := strconv.ParseUint(startTimeStr, 10, 64)
	if err == nil {
		ctime := (t / uint64(ClockTicks)) + p.bootTime
		sInfo.createTime = int64(ctime * 1000)
	}

	return sInfo
}

// ensurePathReadable ensures that the current user is able to read the path before opening it.
// On some systems, attempting to open a file that the user does not have permission is problematic for
// customer security auditing. What we do here is:
// 1. If the agent is running as root (real or via sudo), allow the request
// 2. If the file is a not a symlink and has the other-readable permission bit set, allow the request
// 3. If the owner of the file/link is the current user or effective user, allow the request.
func (p *Probe) ensurePathReadable(path string) error {
	// User is (effectively or actually) root
	if p.euid == 0 {
		return nil
	}

	// TODO (sk): Provide caching on this!
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	// File mode is world readable and not a symlink
	// If the file is a symlink, the owner check below will cover it
	if mode := info.Mode(); mode&os.ModeSymlink == 0 && mode.Perm()&WorldReadable != 0 {
		return nil
	}

	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		// If file is not owned by the user id or effective user id, return a permission error
		// Group permissions don't come in to play with procfs so we don't bother checking
		if stat.Uid != p.uid && stat.Uid != p.euid {
			return os.ErrPermission
		}
	}

	return nil
}

// trimAndSplitBytes converts the raw command line bytes into a list of strings by trimming and splitting on null bytes
func trimAndSplitBytes(bs []byte) []string {
	var components []string

	// Remove leading null bytes
	i := 0
	for j := 0; j < len(bs); j++ {
		if bs[j] == 0 {
			i++
		} else {
			break
		}
	}

	// Split our stream using the null byte separator
	for j := i; j < len(bs); j++ {
		if bs[j] == 0 {
			components = append(components, string(bs[i:j]))
			i = j + 1

			// If we have successive null bytes, skip them (this will also remove trailing null characters)
			for i < len(bs) && bs[i] == 0 {
				i++
				j++
			}
		}
	}

	// attach the last segment if the string is not ended with null byte
	if i < len(bs) {
		components = append(components, string(bs[i:]))
	}

	return components
}

// bootTime returns the system boot time expressed in seconds since the epoch.
// the value is extracted from "/proc/stat"
func bootTime(hostProc string) (uint64, error) {
	filePath := filepath.Join(hostProc, "stat")
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		log.Debugf("Unable to read stat file from %s: %s", filePath, err)
		return 0, nil
	}

	index := 0
	btimePrefix := []byte("btime")

	for i, r := range content {
		if r == '\n' {
			if bytes.HasPrefix(content[index:i], btimePrefix) {
				f := strings.Fields(string(content[index:i]))
				if len(f) != 2 {
					return 0, fmt.Errorf("wrong btime format")
				}

				b, err := strconv.ParseInt(f[1], 10, 64)
				if err != nil {
					return 0, err
				}
				return uint64(b), nil
			}
			index = i + 1
		}
	}

	return 0, fmt.Errorf("could not parse btime")
}
