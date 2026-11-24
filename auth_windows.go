package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

func openAuth(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// Allow another proxy invocation to replace or remove this file while serving.
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func replaceAuth(ctx context.Context, src, dst string) error {
	// MoveFileEx may refuse replacement until current readers close, even with delete sharing.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := os.Rename(src, dst)
		if !errors.Is(err, os.ErrPermission) && !errors.Is(err, syscall.Errno(32)) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-ticker.C:
		}
	}
}
