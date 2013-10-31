// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package main

import (
	"fmt"
	"net/rpc"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"launchpad.net/loggo"

	"launchpad.net/juju-core/cmd"
	"launchpad.net/juju-core/worker/uniter/jujuc"

	// Import the providers.
	_ "launchpad.net/juju-core/provider/all"
)

var jujudDoc = `
juju provides easy, intelligent service orchestration on top of environments
such as OpenStack, Amazon AWS, or bare metal. jujud is a component of juju.

https://juju.ubuntu.com/

The jujud command can also forward invocations over RPC for execution by the
juju unit agent. When used in this way, it expects to be called via a symlink
named for the desired remote command, and expects JUJU_AGENT_SOCKET and
JUJU_CONTEXT_ID be set in its environment.
`

var logger = loggo.GetLogger("juju.jujud.main")

func getenv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("%s not set", name)
	}
	return value, nil
}

func getwd() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// jujuCMain uses JUJU_CONTEXT_ID and JUJU_AGENT_SOCKET to ask a running unit agent
// to execute a Command on our behalf. Individual commands should be exposed
// by symlinking the command name to this executable.
func jujuCMain(commandName string, args []string) (code int, err error) {
	code = 1
	contextId, err := getenv("JUJU_CONTEXT_ID")
	if err != nil {
		return
	}
	dir, err := getwd()
	if err != nil {
		return
	}
	req := jujuc.Request{
		ContextId:   contextId,
		Dir:         dir,
		CommandName: commandName,
		Args:        args[1:],
	}
	socketPath, err := getenv("JUJU_AGENT_SOCKET")
	if err != nil {
		return
	}
	client, err := rpc.Dial("unix", socketPath)
	if err != nil {
		return
	}
	defer client.Close()
	var resp jujuc.Response
	err = client.Call("Jujuc.Main", req, &resp)
	if err != nil {
		return
	}
	os.Stdout.Write(resp.Stdout)
	os.Stderr.Write(resp.Stderr)
	return resp.Code, nil
}

func captureMemoryProfile(agentTag string) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	now := time.Now()
	fname := fmt.Sprintf("/tmp/agent-mem-%s-%s-%.3fGB.mprof",
		agentTag, now.Format("2006-01-02-15_04_05"),
		float64(m.Alloc)/(1024.0*1024*1024))
	f, err := os.Create(fname)
	if err != nil {
		return
	}
	logger.Debugf("logging memory profile to %s", fname)
	pprof.WriteHeapProfile(f)
	f.Close()
}

func profileMemory(agentTag string, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(1 * time.Minute):
			captureMemoryProfile(agentTag)
		}
	}
}

// enable the CPU and Memory profiling for this agent
func enableProfiling(agentTag string) func() {
	var fname string
	for i := 0; i < 10; i++ {
		fname = fmt.Sprintf("/tmp/agent-cpu-%s-%d.prof", agentTag, i)
		if f, err := os.Open(fname); err == nil {
			// File already exists, don't use it
			logger.Debugf("not using %s for cpu logging file already exists", fname)
			f.Close()
		} else {
			break
		}
	}
	f, err := os.Create(fname)
	if err != nil {
		logger.Warningf("error creating cpu profiling file: %s: %s", fname, err)
		return func() {}
	}
	err = pprof.StartCPUProfile(f)
	if err != nil {
		logger.Warningf("Failed to start CPU Profiling: %s", err)
	} else {
		logger.Debugf("logging CPU profile to %s", fname)
	}
	stopChan := make(chan struct{})
	go profileMemory(agentTag, stopChan)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGUSR1, syscall.SIGTERM)
	go func() {
		for sig := range signalChan {
			logger.Infof("got signal: %v, dumping profiles", sig)
			pprof.StopCPUProfile()
			captureMemoryProfile(agentTag)
			os.Exit(1)
		}
	}()
	return func() {
		close(stopChan)
		logger.Debugf("flushing CPUProfile")
		pprof.StopCPUProfile()
		f.Close()
	}
}

// Main registers subcommands for the jujud executable, and hands over control
// to the cmd package.
func jujuDMain(args []string) (code int, err error) {
	logger.Debugf("setting GOMAXPROCS = %d", runtime.NumCPU())
	runtime.GOMAXPROCS(runtime.NumCPU())
	jujud := cmd.NewSuperCommand(cmd.SuperCommandParams{
		Name: "jujud",
		Doc:  jujudDoc,
		Log:  &cmd.Log{},
	})
	jujud.Register(&BootstrapCommand{})
	jujud.Register(&MachineAgent{})
	jujud.Register(&UnitAgent{})
	jujud.Register(&cmd.VersionCommand{})
	code = cmd.Main(jujud, cmd.DefaultContext(), args[1:])
	return code, nil
}

// Main is not redundant with main(), because it provides an entry point
// for testing with arbitrary command line arguments.
func Main(args []string) {
	var code int = 1
	var err error
	commandName := filepath.Base(args[0])
	if commandName == "jujud" {
		code, err = jujuDMain(args)
	} else if commandName == "jujuc" {
		fmt.Fprint(os.Stderr, jujudDoc)
		code = 2
		err = fmt.Errorf("jujuc should not be called directly")
	} else {
		code, err = jujuCMain(commandName, args)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	os.Exit(code)
}

func main() {
	Main(os.Args)
}
