//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	pipeAccessDuplex   = 0x00000003
	pipeTypeByte       = 0x00000000
	pipeReadmodeByte   = 0x00000000
	pipeWait           = 0x00000000
	pipeUnlimited      = 255
	pipeFirstInstance  = 0x00080000
	pipeBufferBytes    = 64 << 10
	errorFileNotFound  = 2
	errorPipeBusy      = 231
	errorPipeConnected = 535

	tokenQuery           = 0x0008
	tokenUserInformation = 1
	sddlRevision1        = 1
	genericAllPipeRights = "GA"
)

var (
	procCreateNamedPipeW = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe = kernel32.NewProc("ConnectNamedPipe")
	procLocalFree        = kernel32.NewProc("LocalFree")

	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procGetTokenInformation                                  = advapi32.NewProc("GetTokenInformation")
	procConvertSidToStringSidW                               = advapi32.NewProc("ConvertSidToStringSidW")
	procConvertStringSecurityDescriptorToSecurityDescriptorW = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
)

// endpointName is a named pipe. Go's AF_UNIX support on Windows binds but cannot
// connect across processes, so a pipe is the working local endpoint here. The
// pipe namespace is machine-wide, so the user name keeps two signed-in users
// apart. A pipe created without a security descriptor inherits the creating
// process's default DACL, which grants the owning user, SYSTEM, and
// Administrators only.
func endpointName() string {
	if p := strings.TrimSpace(os.Getenv("OCX_CODEX_BRIDGE_ENDPOINT")); p != "" {
		return p
	}
	user := strings.TrimSpace(os.Getenv("USERNAME"))
	if user == "" {
		user = "default"
	}
	return `\\.\pipe\opencodex-codex-appserver-bridge-` + sanitizePipeName(user)
}

func sanitizePipeName(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, value)
}

type pipeListener struct {
	name       string
	attributes *syscall.SecurityAttributes
	mu         sync.Mutex
	first      bool
	closed     bool
}

func listenLocal(name string) (localListener, error) {
	attributes, err := pipeSecurityAttributes()
	if err != nil {
		return nil, err
	}
	return &pipeListener{name: name, attributes: attributes, first: true}, nil
}

// Accept creates one pipe instance and blocks until a client connects. Instances
// are published one at a time, which is enough for short command round trips.
func (l *pipeListener) Accept() (localConn, error) {
	l.mu.Lock()
	first, closed := l.first, l.closed
	l.first = false
	l.mu.Unlock()
	if closed {
		return nil, io.EOF
	}
	handle, err := createPipeInstance(l.name, first, l.attributes)
	if err != nil {
		return nil, err
	}
	if err := connectPipe(handle); err != nil {
		syscall.CloseHandle(handle)
		return nil, err
	}
	return &pipeConn{handle: handle}, nil
}

func (l *pipeListener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return nil
}

func createPipeInstance(name string, first bool, attributes *syscall.SecurityAttributes) (syscall.Handle, error) {
	pointer, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	openMode := uint32(pipeAccessDuplex)
	if first {
		// Refuse to silently stack a second bridge behind a live one.
		openMode |= pipeFirstInstance
	}
	attributesPointer := uintptr(0)
	if attributes != nil {
		attributesPointer = uintptr(unsafe.Pointer(attributes))
	}
	result, _, callErr := procCreateNamedPipeW.Call(
		uintptr(unsafe.Pointer(pointer)),
		uintptr(openMode),
		uintptr(pipeTypeByte|pipeReadmodeByte|pipeWait),
		uintptr(pipeUnlimited),
		uintptr(pipeBufferBytes),
		uintptr(pipeBufferBytes),
		0,
		attributesPointer,
	)
	if syscall.Handle(result) == syscall.InvalidHandle {
		return 0, callErr
	}
	return syscall.Handle(result), nil
}

// pipeSecurityAttributes builds the DACL for the pipe explicitly: the current
// user, SYSTEM, and Administrators, with inheritance disabled. A pipe created
// without a descriptor inherits whatever the creating token's default DACL
// happens to be, which is not a contract this component should depend on.
//
// The descriptor is deliberately never freed: it is referenced by every pipe
// instance for the life of the process, and freeing it while Accept is inside
// CreateNamedPipe would be a use-after-free.
func pipeSecurityAttributes() (*syscall.SecurityAttributes, error) {
	token, err := openCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(syscall.Handle(token))

	sid, err := tokenUserSidString(token)
	if err != nil {
		return nil, err
	}
	descriptor, err := securityDescriptorFromSDDL("D:P(A;;" + genericAllPipeRights + ";;;SY)(A;;" + genericAllPipeRights + ";;;BA)(A;;" + genericAllPipeRights + ";;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	if debugEnabled {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge debug: pipe DACL D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)\n", sid)
	}
	return &syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
		InheritHandle:      0,
	}, nil
}

func openCurrentProcessToken() (syscall.Token, error) {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, err
	}
	var token syscall.Token
	if err := syscall.OpenProcessToken(process, tokenQuery, &token); err != nil {
		return 0, err
	}
	return token, nil
}

// tokenUserSidString reads the token's TOKEN_USER and converts the SID to its
// string form. The TOKEN_USER buffer must stay reachable while the SID pointer
// inside it is used, so the conversion happens before this function returns.
func tokenUserSidString(token syscall.Token) (string, error) {
	var needed uint32
	_, _, _ = procGetTokenInformation.Call(
		uintptr(token),
		tokenUserInformation,
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
	)
	if needed == 0 {
		return "", errors.New("could not size the process token user information")
	}
	buffer := make([]byte, needed)
	ok, _, callErr := procGetTokenInformation.Call(
		uintptr(token),
		tokenUserInformation,
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
	)
	if ok == 0 {
		return "", callErr
	}
	// TOKEN_USER starts with SID_AND_ATTRIBUTES, whose first field is the SID.
	sid := *(*uintptr)(unsafe.Pointer(&buffer[0]))

	var stringPointer *uint16
	ok, _, callErr = procConvertSidToStringSidW.Call(sid, uintptr(unsafe.Pointer(&stringPointer)))
	if ok == 0 {
		return "", callErr
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(stringPointer)))

	length := 0
	for pointer := unsafe.Pointer(stringPointer); *(*uint16)(pointer) != 0; pointer = unsafe.Add(pointer, unsafe.Sizeof(uint16(0))) {
		length++
	}
	return syscall.UTF16ToString(unsafe.Slice(stringPointer, length)), nil
}

func securityDescriptorFromSDDL(sddl string) (uintptr, error) {
	pointer, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return 0, err
	}
	var descriptor uintptr
	ok, _, callErr := procConvertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(pointer)),
		sddlRevision1,
		uintptr(unsafe.Pointer(&descriptor)),
		0,
	)
	if ok == 0 {
		return 0, callErr
	}
	return descriptor, nil
}

func connectPipe(handle syscall.Handle) error {
	ok, _, callErr := procConnectNamedPipe.Call(uintptr(handle), 0)
	if ok != 0 {
		return nil
	}
	// A client that connected between CreateNamedPipe and ConnectNamedPipe makes
	// ConnectNamedPipe fail with ERROR_PIPE_CONNECTED, which is success here.
	if errno, isErrno := callErr.(syscall.Errno); isErrno && errno == errorPipeConnected {
		return nil
	}
	return callErr
}

type pipeConn struct {
	handle syscall.Handle
}

func (c *pipeConn) Read(buffer []byte) (int, error) {
	var done uint32
	if err := syscall.ReadFile(c.handle, buffer, &done, nil); err != nil {
		return int(done), err
	}
	if done == 0 {
		return 0, io.EOF
	}
	return int(done), nil
}

func (c *pipeConn) Write(buffer []byte) (int, error) {
	var done uint32
	if err := syscall.WriteFile(c.handle, buffer, &done, nil); err != nil {
		return int(done), err
	}
	return int(done), nil
}

func (c *pipeConn) Close() error {
	return syscall.CloseHandle(c.handle)
}

// dialLocal retries briefly. CreateFile fails immediately when no instance is
// listening, and the server has a short gap between accepting one client and
// publishing the next instance.
func dialLocal(name string) (localConn, error) {
	pointer, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		handle, err := syscall.CreateFile(
			pointer,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			0,
			nil,
			syscall.OPEN_EXISTING,
			0,
			0,
		)
		if err == nil {
			return &pipeConn{handle: handle}, nil
		}
		lastErr = err
		errno, isErrno := err.(syscall.Errno)
		if !isErrno || (errno != errorFileNotFound && errno != errorPipeBusy) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, lastErr
}
