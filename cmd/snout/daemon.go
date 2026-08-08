package main

import (
	"fmt"
	"os"
	"syscall"
)

func daemonize(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devNull.Close()
	workingDirectory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}

	childArgs := append([]string{executable}, args...)
	childArgs = append(childArgs, "-f")
	process, err := os.StartProcess(executable, childArgs, &os.ProcAttr{
		Dir:   workingDirectory,
		Env:   os.Environ(),
		Files: []*os.File{devNull, devNull, devNull},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	return process.Release()
}
