package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const binaryPath = "./tmp/minidock"

type process struct {
	command *exec.Cmd
}

func main() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fatal(err)
	}
	defer watcher.Close()

	if err := watchTree(watcher, "."); err != nil {
		fatal(err)
	}

	server := &process{}
	if err := server.restart(); err != nil {
		fmt.Fprintln(os.Stderr, "initial build failed; watching for changes")
	}
	defer server.stop()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	var timer *time.Timer
	var rebuild <-chan time.Time
	for {
		select {
		case <-interrupt:
			return
		case err := <-watcher.Errors:
			if err != nil {
				fmt.Fprintln(os.Stderr, "watch error:", err)
			}
		case event := <-watcher.Events:
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() && !ignored(event.Name) {
					if err := watchTree(watcher, event.Name); err != nil {
						fmt.Fprintln(os.Stderr, "watch directory:", err)
					}
				}
			}
			if relevant(event.Name) {
				if timer != nil {
					timer.Stop()
				}
				timer = time.NewTimer(350 * time.Millisecond)
				rebuild = timer.C
			}
		case <-rebuild:
			if err := server.restart(); err != nil {
				fmt.Fprintln(os.Stderr, "rebuild failed; keeping the current server running")
			}
			timer = nil
			rebuild = nil
		}
	}
}

func (p *process) restart() error {
	build := exec.Command("go", "build", "-o", binaryPath, "./cmd/minidock")
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return err
	}
	p.stop()

	p.command = exec.Command(binaryPath)
	p.command.Env = os.Environ()
	p.command.Stdout = os.Stdout
	p.command.Stderr = os.Stderr
	p.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := p.command.Start(); err != nil {
		return err
	}
	fmt.Printf("started MiniDock (pid %d)\n", p.command.Process.Pid)
	return nil
}

func (p *process) stop() {
	if p.command == nil || p.command.Process == nil || p.command.ProcessState != nil && p.command.ProcessState.Exited() {
		return
	}
	_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.command.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
		<-done
	}
	p.command = nil
}

func watchTree(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && !ignored(path) {
			return watcher.Add(path)
		}
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

func ignored(path string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == ".git" || part == "data" || part == "tmp" {
			return true
		}
	}
	return false
}

func relevant(path string) bool {
	if ignored(path) {
		return false
	}
	extension := filepath.Ext(path)
	return extension == ".go" || extension == ".html"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
