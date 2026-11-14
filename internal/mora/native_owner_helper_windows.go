//go:build windows

package mora

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsNativeOwnerPeer struct{ id string }

func (p windowsNativeOwnerPeer) identity() (string, bool) { return p.id, p.id != "" }

type unavailableWindowsNativeOwnerPeer struct{}

func (unavailableWindowsNativeOwnerPeer) identity() (string, bool) { return "", false }

func newNativeOwnerVerifier(cfg Config) desktopReviewVerifier {
	// Direct calls never authenticate Windows. The command path below creates the
	// helper and replaces its stdin/stdout with Go-owned private handles.
	return newNativeOwnerVerifierForPeer(cfg, unavailableWindowsNativeOwnerPeer{})
}

func serveNativeDesktopReview(ctx context.Context, cfg Config, _ io.Reader, _ io.Writer) error {
	if nativeOwnerWindowsHelperSHA256 == "" {
		return errDesktopReviewProofUnavailable
	}
	helper, handle, identity, err := nativeOwnerOpenPinnedWindowsHelper()
	if err != nil {
		return errDesktopReviewProofUnavailable
	}
	defer handle.Close()
	stdin, stdout, stderr, process, err := nativeOwnerStartWindowsHelper(helper)
	if err != nil {
		return errDesktopReviewProofUnavailable
	}
	defer process.Close()
	defer stdin.Close()
	defer stdout.Close()
	defer stderr.Close()
	go func() { _, _ = io.Copy(io.Discard, io.LimitReader(stderr, desktopReviewFrameLimit)) }()
	verifier := newNativeOwnerVerifierForPeer(cfg, windowsNativeOwnerPeer{id: identity})
	done := make(chan error, 1)
	go func() { done <- serveDesktopReviewStdio(ctx, cfg, verifier, stdout, stdin) }()
	select {
	case err := <-done:
		_ = windows.TerminateProcess(windows.Handle(process.Fd()), 1)
		return err
	case <-ctx.Done():
		_ = windows.TerminateProcess(windows.Handle(process.Fd()), 1)
		return ctx.Err()
	}
}

func nativeOwnerOpenPinnedWindowsHelper() (string, *os.File, string, error) {
	if nativeOwnerWindowsHelperSHA256 == "" {
		return "", nil, "", errors.New("missing helper digest")
	}
	self, err := os.Executable()
	if err != nil {
		return "", nil, "", err
	}
	path := filepath.Join(filepath.Dir(self), nativeOwnerWindowsHelperFilename)
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return "", nil, "", err
	}
	f := os.NewFile(uintptr(h), path)
	if err := nativeOwnerHelperDigestMatches(f, nativeOwnerWindowsHelperSHA256); err != nil {
		f.Close()
		return "", nil, "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return "", nil, "", err
	}
	return path, f, "sha256:" + nativeOwnerWindowsHelperSHA256, nil
}

// nativeOwnerStartWindowsHelper uses a handle list rather than inheriting the
// authority process's handle table. Only the three child stdio handles cross.
func nativeOwnerStartWindowsHelper(path string) (stdin io.WriteCloser, stdout, stderr io.ReadCloser, process *os.File, err error) {
	var childIn, parentIn, parentOut, childOut, parentErr, childErr windows.Handle
	closeAll := func() {
		for _, h := range []windows.Handle{childIn, parentIn, parentOut, childOut, parentErr, childErr} {
			if h != 0 {
				_ = windows.CloseHandle(h)
			}
		}
	}
	attr, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer attr.Delete()
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	if err = windows.CreatePipe(&childIn, &parentIn, sa, 0); err != nil {
		return nil, nil, nil, nil, err
	}
	if err = windows.CreatePipe(&parentOut, &childOut, sa, 0); err != nil {
		closeAll()
		return nil, nil, nil, nil, err
	}
	if err = windows.CreatePipe(&parentErr, &childErr, sa, 0); err != nil {
		closeAll()
		return nil, nil, nil, nil, err
	}
	for _, h := range []windows.Handle{parentIn, parentOut, parentErr} {
		if err = windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			closeAll()
			return nil, nil, nil, nil, err
		}
	}
	handles := []windows.Handle{childIn, childOut, childErr}
	if err = attr.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		closeAll()
		return nil, nil, nil, nil, err
	}
	app, err := windows.UTF16PtrFromString(path)
	if err != nil {
		closeAll()
		return nil, nil, nil, nil, err
	}
	cmd, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{path, "--stdio"}))
	if err != nil {
		closeAll()
		return nil, nil, nil, nil, err
	}
	si := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES, StdInput: childIn, StdOutput: childOut, StdErr: childErr}, ProcThreadAttributeList: attr.List()}
	pi := new(windows.ProcessInformation)
	err = windows.CreateProcess(app, cmd, nil, nil, true, windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT, nil, nil, &si.StartupInfo, pi)
	if err != nil {
		closeAll()
		return nil, nil, nil, nil, err
	}
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(childIn)
	_ = windows.CloseHandle(childOut)
	_ = windows.CloseHandle(childErr)
	return os.NewFile(uintptr(parentIn), "mora-review-stdin"), os.NewFile(uintptr(parentOut), "mora-review-stdout"), os.NewFile(uintptr(parentErr), "mora-review-stderr"), os.NewFile(uintptr(pi.Process), "mora-review-process"), nil
}
