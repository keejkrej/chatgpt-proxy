//go:build !windows

package main

import (
	"context"
	"os"
)

func openAuth(path string) (*os.File, error) { return os.Open(path) }

func replaceAuth(_ context.Context, src, dst string) error { return os.Rename(src, dst) }
