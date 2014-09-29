package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"time"

	rpc "github.com/conformal/btcrpcclient"
)

// ErrConnectionTimeOut is raised when a rpc client is unable to connect
// to the node within maxConnRetries * 50ms
var ErrConnectionTimeOut = errors.New("connection timeout")

// Args is an interface which specifies how to access all the data required
// to launch and connect to a RPC server, typically btcd or btcwallet
type Args interface {
	Arguments() []string
	Command() *exec.Cmd
	RPCConnConfig() rpc.ConnConfig
	Cleanup() error
	fmt.Stringer
}

// Node is a RPC server node, typically btcd or btcwallet and functions to
// manage the instance
// All functions common to btcd and btcwallet go here while btcdArgs and
// btcwalletArgs hold the different implementations
type Node struct {
	Args
	handlers *rpc.NotificationHandlers
	cmd      *exec.Cmd
	client   *rpc.Client
}

// NewNodeFromArgs starts a new node using the args provided, sets the handlers
// and loggers. It does not start the node process, Start() should be called for that
func NewNodeFromArgs(args Args, handlers *rpc.NotificationHandlers, w io.Writer) (*Node, error) {
	n := Node{
		Args:     args,
		handlers: handlers,
	}
	cmd := n.Command()
	n.cmd = cmd
	if w != nil {
		n.cmd.Stdout = w
		n.cmd.Stderr = w
	}
	return &n, nil
}

// Start stats the node command
func (n *Node) Start() error {
	return n.cmd.Start()
}

// Connect tries to connect to the launched node and sets the
// client field. It returns an error if the connection times out
func (n *Node) Connect() error {
	var client *rpc.Client
	var err error

	rpcConf := n.RPCConnConfig()

	for i := 0; i < *maxConnRetries; i++ {
		if client, err = rpc.New(&rpcConf, n.handlers); err != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		break
	}
	if client == nil {
		return ErrConnectionTimeOut
	}
	n.client = client
	return nil
}

// Stop interrupts a process and waits until it exits
// On windows, interrupt is not supported, so a kill
// signal is used instead
func (n *Node) Stop() error {
	if n.cmd == nil || n.cmd.Process == nil {
		// return if not properly initialized
		// or error starting the process
		return nil
	}
	defer n.cmd.Wait()
	if runtime.GOOS == "windows" {
		return n.cmd.Process.Signal(os.Kill)
	}
	return n.cmd.Process.Signal(os.Interrupt)
}

// Shutdown stops a node and cleansup
func (n *Node) Shutdown() {
	if n.client != nil {
		n.client.Shutdown()
	}
	if err := n.Stop(); err != nil {
		log.Printf("%s: Cannot stop node: %v", n, err)
	}
	if err := n.Cleanup(); err != nil {
		log.Printf("%s: Cannot cleanup: %v", n, err)
	}
	log.Printf("%s: Shutdown", n)
}
