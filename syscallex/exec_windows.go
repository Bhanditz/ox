package syscallex

import (
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// makeCmdLine builds a command line out of args by escaping "special"
// characters and joining the arguments with spaces.
func makeCmdLine(args []string) string {
	var s string
	for _, v := range args {
		if s != "" {
			s += " "
		}
		s += syscall.EscapeArg(v)
	}
	return s
}

// createEnvBlock converts an array of environment strings into
// the representation required by CreateProcess: a sequence of NUL
// terminated strings followed by a nil.
// Last bytes are two UCS-2 NULs, or four NUL bytes.
func createEnvBlock(envv []string) *uint16 {
	if len(envv) == 0 {
		return &utf16.Encode([]rune("\x00\x00"))[0]
	}
	length := 0
	for _, s := range envv {
		length += len(s) + 1
	}
	length += 1

	b := make([]byte, length)
	i := 0
	for _, s := range envv {
		l := len(s)
		copy(b[i:i+l], []byte(s))
		copy(b[i+l:i+l+1], []byte{0})
		i = i + l + 1
	}
	copy(b[i:i+1], []byte{0})

	return &utf16.Encode([]rune(string(b)))[0]
}

func isSlash(c uint8) bool {
	return c == '\\' || c == '/'
}

func normalizeDir(dir string) (name string, err error) {
	ndir, err := syscall.FullPath(dir)
	if err != nil {
		return "", err
	}
	if len(ndir) > 2 && isSlash(ndir[0]) && isSlash(ndir[1]) {
		// dir cannot have \\server\share\path form
		return "", syscall.EINVAL
	}
	return ndir, nil
}

func volToUpper(ch int) int {
	if 'a' <= ch && ch <= 'z' {
		ch += 'A' - 'a'
	}
	return ch
}

func joinExeDirAndFName(dir, p string) (name string, err error) {
	if len(p) == 0 {
		return "", syscall.EINVAL
	}
	if len(p) > 2 && isSlash(p[0]) && isSlash(p[1]) {
		// \\server\share\path form
		return p, nil
	}
	if len(p) > 1 && p[1] == ':' {
		// has drive letter
		if len(p) == 2 {
			return "", syscall.EINVAL
		}
		if isSlash(p[2]) {
			return p, nil
		} else {
			d, err := normalizeDir(dir)
			if err != nil {
				return "", err
			}
			if volToUpper(int(p[0])) == volToUpper(int(d[0])) {
				return syscall.FullPath(d + "\\" + p[2:])
			} else {
				return syscall.FullPath(p)
			}
		}
	} else {
		// no drive letter
		d, err := normalizeDir(dir)
		if err != nil {
			return "", err
		}
		if isSlash(p[0]) {
			return syscall.FullPath(d[:2] + p)
		} else {
			return syscall.FullPath(d + "\\" + p)
		}
	}
}

type ProcAttr struct {
	Dir   string
	Env   []string
	Files []uintptr
	Sys   *SysProcAttr
}

type SysProcAttr struct {
	HideWindow    bool
	CmdLine       string // used if non-empty, else the windows command line is built by escaping the arguments passed to StartProcess
	CreationFlags uint32
	LogonFlags    uint32
	ProcessHandle syscall.Handle
	ThreadHandle  syscall.Handle
}

var zeroProcAttr ProcAttr
var zeroSysProcAttr SysProcAttr

func StartProcessWithLogon(argv0 string, argv []string, username string, domain string, password string, attr *ProcAttr) (pid int, handle uintptr, err error) {
	if len(argv0) == 0 {
		return 0, 0, syscall.EWINDOWS
	}
	if attr == nil {
		attr = &zeroProcAttr
	}
	sys := attr.Sys
	if sys == nil {
		sys = &zeroSysProcAttr
	}

	if len(attr.Files) > 3 {
		return 0, 0, syscall.EWINDOWS
	}
	if len(attr.Files) < 3 {
		return 0, 0, syscall.EINVAL
	}

	if len(attr.Dir) != 0 {
		// StartProcess assumes that argv0 is relative to attr.Dir,
		// because it implies Chdir(attr.Dir) before executing argv0.
		// Windows CreateProcess assumes the opposite: it looks for
		// argv0 relative to the current directory, and, only once the new
		// process is started, it does Chdir(attr.Dir). We are adjusting
		// for that difference here by making argv0 absolute.
		var err error
		argv0, err = joinExeDirAndFName(attr.Dir, argv0)
		if err != nil {
			return 0, 0, err
		}
	}
	argv0p, err := syscall.UTF16PtrFromString(argv0)
	if err != nil {
		return 0, 0, err
	}

	var cmdline string
	// Windows CreateProcess takes the command line as a single string:
	// use attr.CmdLine if set, else build the command line by escaping
	// and joining each argument with spaces
	if sys.CmdLine != "" {
		cmdline = sys.CmdLine
	} else {
		cmdline = makeCmdLine(argv)
	}

	var argvp *uint16
	if len(cmdline) != 0 {
		argvp, err = syscall.UTF16PtrFromString(cmdline)
		if err != nil {
			return 0, 0, err
		}
	}

	var dirp *uint16
	if len(attr.Dir) != 0 {
		dirp, err = syscall.UTF16PtrFromString(attr.Dir)
		if err != nil {
			return 0, 0, err
		}
	}

	// Acquire the fork lock so that no other threads
	// create new fds that are not yet close-on-exec
	// before we fork.
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()

	p, _ := syscall.GetCurrentProcess()
	fd := make([]syscall.Handle, len(attr.Files))
	for i := range attr.Files {
		if attr.Files[i] > 0 {
			err := syscall.DuplicateHandle(p, syscall.Handle(attr.Files[i]), p, &fd[i], 0, true, syscall.DUPLICATE_SAME_ACCESS)
			if err != nil {
				return 0, 0, err
			}
			defer syscall.CloseHandle(syscall.Handle(fd[i]))
		}
	}
	si := new(syscall.StartupInfo)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = syscall.STARTF_USESTDHANDLES
	if sys.HideWindow {
		si.Flags |= syscall.STARTF_USESHOWWINDOW
		si.ShowWindow = syscall.SW_HIDE
	}
	si.StdInput = fd[0]
	si.StdOutput = fd[1]
	si.StdErr = fd[2]

	if sys.CreationFlags&CREATE_NEW_CONSOLE != 0 {
		si.Flags &^= syscall.STARTF_USESTDHANDLES
		si.StdInput = 0
		si.StdOutput = 0
		si.StdErr = 0
	}

	pi := new(syscall.ProcessInformation)

	if username == "" {
		creationFlags := sys.CreationFlags | syscall.CREATE_UNICODE_ENVIRONMENT
		err = syscall.CreateProcess(
			argv0p, argvp, nil, nil, true, creationFlags, createEnvBlock(attr.Env), dirp, si, pi,
		)
		if err != nil {
			return 0, 0, err
		}
	} else {
		usernamep := syscall.StringToUTF16Ptr(username)
		domainp := syscall.StringToUTF16Ptr(domain)
		passwordp := syscall.StringToUTF16Ptr(password)

		creationFlags := sys.CreationFlags | syscall.CREATE_UNICODE_ENVIRONMENT
		logonFlags := sys.LogonFlags
		err = CreateProcessWithLogon(
			usernamep, domainp, passwordp, logonFlags,
			argv0p, argvp, creationFlags, createEnvBlock(attr.Env), dirp, si, pi,
		)
		if err != nil {
			return 0, 0, err
		}
	}
	attr.Sys.ThreadHandle = syscall.Handle(pi.Thread)
	attr.Sys.ProcessHandle = syscall.Handle(pi.Process)

	return int(pi.ProcessId), uintptr(pi.Process), nil
}
