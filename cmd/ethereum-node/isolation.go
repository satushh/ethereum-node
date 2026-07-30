package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/satushh/ethereum-node/internal/gethapp"
)

// runProcessIsolated runs the two modules as supervised child processes of
// this same binary: identical code and identical resolved configuration to
// single-process mode, with only the process boundary changed. This restores
// crash isolation (a fault in one module no longer takes down the other) and
// doubles as the A/B harness for measuring what sharing one process buys —
// the engine API rides the same in-datadir IPC socket in both modes.
func runProcessIsolated(gethCfg gethapp.Config, beaconArgs []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cfgJSON, err := json.Marshal(gethCfg)
	if err != nil {
		return err
	}

	// Children get their own process groups so terminal signals reach only
	// the supervisor, which enforces the same shutdown order as
	// single-process mode: beacon drains first, then geth persists.
	el := exec.Command(self, "el-child", "--config-json="+string(cfgJSON))
	el.Stdout, el.Stderr = os.Stdout, os.Stderr
	el.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := el.Start(); err != nil {
		return fmt.Errorf("start execution child: %w", err)
	}
	fmt.Printf("ethereum-node: execution child started (pid %d)\n", el.Process.Pid)

	ipcPath := filepath.Join(gethCfg.DataDir, "geth.ipc")
	elDone := make(chan error, 1)
	go func() { elDone <- el.Wait() }()
	if err := waitForFile(ipcPath, 90*time.Second, elDone); err != nil {
		_ = el.Process.Signal(syscall.SIGTERM)
		<-elDone
		return fmt.Errorf("execution child never exposed the engine socket: %w", err)
	}

	cl := exec.Command(self, append([]string{"beacon"}, beaconArgs[1:]...)...)
	cl.Stdout, cl.Stderr = os.Stdout, os.Stderr
	cl.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cl.Start(); err != nil {
		_ = el.Process.Signal(syscall.SIGTERM)
		<-elDone
		return fmt.Errorf("start consensus child: %w", err)
	}
	fmt.Printf("ethereum-node: consensus child started (pid %d)\n", cl.Process.Pid)

	clDone := make(chan error, 1)
	go func() { clDone <- cl.Wait() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	stopEL := func() error {
		fmt.Println("ethereum-node: stopping execution child")
		_ = el.Process.Signal(syscall.SIGTERM)
		return <-elDone
	}
	select {
	case <-sig:
		_ = cl.Process.Signal(syscall.SIGTERM)
		<-clDone
		return stopEL()
	case err := <-clDone:
		fmt.Fprintln(os.Stderr, "ethereum-node: consensus child exited")
		if stopErr := stopEL(); err == nil {
			err = stopErr
		}
		return err
	case err := <-elDone:
		fmt.Fprintln(os.Stderr, "ethereum-node: execution child exited; stopping consensus child")
		_ = cl.Process.Signal(syscall.SIGTERM)
		<-clDone
		if err == nil {
			err = fmt.Errorf("execution child exited unexpectedly")
		}
		return err
	}
}

// waitForFile polls for path to exist, failing early if the process being
// waited on dies first.
func waitForFile(path string, timeout time.Duration, died <-chan error) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case err := <-died:
			return fmt.Errorf("process exited while waiting: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out after %s waiting for %s", timeout, path)
}

// elChildCommand is the hidden entrypoint for the execution module under
// --isolation=process: gethapp with the exact config the supervisor
// resolved, running until SIGTERM.
func elChildCommand() *cli.Command {
	return &cli.Command{
		Name:   "el-child",
		Usage:  "internal: execution module child for run --isolation=process",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config-json", Required: true},
		},
		Action: func(c *cli.Context) error {
			var cfg gethapp.Config
			if err := json.Unmarshal([]byte(c.String("config-json")), &cfg); err != nil {
				return fmt.Errorf("decode execution child config: %w", err)
			}
			n, err := gethapp.Start(cfg)
			if err != nil {
				return err
			}
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			<-sig
			return n.Close()
		},
	}
}
