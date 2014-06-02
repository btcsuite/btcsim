// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
	//"github.com/conformal/btcwire"
)

// ChainServer describes the arguments necessary to connect a btcwallet
// instance to a btcd websocket RPC server.
type ChainServer struct {
	connect  string
	user     string
	pass     string
	certPath string
	keyPath  string
	cert     []byte
}

// Actor describes an actor on the simulation network.  Each actor runs
// independantly without external input to decide it's behavior.
type Actor struct {
	args   procArgs
	cmd    *exec.Cmd
	client *rpc.Client

	// Parameters specifying how actor behaves should be included here.

	quit chan struct{}
	wg   sync.WaitGroup
}

// These constants define the strings used for actor's authentication and
// wallet encryption.  They are not needed to be secure, and are shared
// between all actors.
const (
	actorWalletPassphrase = "banana"
	actorRPCUser          = "michalis"
	actorRPCPass          = "kbxkwb"

	addressNum  = 10
	timeoutSecs = int64(3600 * 24)
	txPerSec    = 3
)

var (
	connected    = make(chan struct{})
	ntfnHandlers = rpc.NotificationHandlers{
		OnBtcdConnected: func(conn bool) {
			if conn {
				connected <- struct{}{}
			}
		},
	}
)

// NewActor creates a new actor which runs its own wallet process connecting
// to the btcd chain server specified by chain, and listening for simulator
// websocket connections on the specified port.
func NewActor(chain *ChainServer, port uint16) (*Actor, error) {
	// Please don't run this as root.
	if port < 1024 {
		return nil, errors.New("invalid actor port")
	}

	dir, err := ioutil.TempDir("", "actor")
	if err != nil {
		return nil, err
	}

	a := Actor{
		args: procArgs{
			chainSvr:         *chain,
			dir:              dir,
			port:             strconv.FormatUint(uint64(port), 10),
			rpcUser:          actorRPCUser,
			rpcPass:          actorRPCPass,
			walletPassphrase: actorWalletPassphrase,
		},
		quit: make(chan struct{}),
	}
	return &a, nil
}

// Start creates the command to execute a wallet process and starts the
// command in the background, attaching the command's stderr and stdout
// to the passed writers.  Nil writers may be used to discard output.
//
// In addition to starting the wallet process, this runs goroutines to
// handle wallet notifications (TODO: actually do this) and requests
// the wallet process to create an intial encrypted wallet, so that it
// can actually send and receive BTC.
//
// If the RPC client connction cannot be established or wallet cannot
// be created, the wallet process is killed and the actor directory
// removed.
func (a *Actor) Start(stderr, stdout io.Writer) error {
	// Overwriting the previously created command would be sad.
	if a.cmd != nil {
		return errors.New("actor command previously created")
	}

	// Create and start command in background.
	a.cmd = a.args.Cmd()
	a.cmd.Stdout = stderr
	a.cmd.Stderr = stdout
	if err := a.cmd.Start(); err != nil {
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot remove actor directory after "+
				"failed start of wallet process: %v", err)
		}
		return err
	}

	// Create and start RPC client.
	rpcConf := rpc.ConnConfig{
		Host:         "localhost:" + a.args.port,
		Endpoint:     "frontend",
		User:         a.args.rpcUser,
		Pass:         a.args.rpcPass,
		Certificates: a.args.chainSvr.cert,
	}

	// The RPC client will not wait for the RPC server to start up, so
	// loop a few times and attempt additional connections, sleeping
	// after each failure.
	var client *rpc.Client
	var connErr error
	for i := 0; i < 5; i++ {
		if client, connErr = rpc.New(&rpcConf, &ntfnHandlers); connErr != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		a.client = client
		break
	}
	if a.client == nil {
		if err := a.cmd.Process.Kill(); err != nil {
			log.Printf("Cannot kill wallet process after failed "+
				"client connect: %v", err)
		}
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot remove actor directory after "+
				"failed client connect: %v", err)
		}
		return connErr
	}

	<-connected

	// Create the wallet.
	if err := a.client.CreateEncryptedWallet(a.args.walletPassphrase); err != nil {
		if err := a.cmd.Process.Kill(); err != nil {
			log.Printf("Cannot kill wallet process after failed "+
				"wallet creation: %v", err)
		}
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot remove actor directory after "+
				"failed wallet creation: %v", err)
		}
		return err
	}

	// Create wallet addresses.
	addressSpace := make([]btcutil.Address, addressNum)
	for i := 0; i < addressNum; i++ {
		a.wg.Add(1)
		go func(i int) {
			defer a.wg.Done()
			addr, err := client.GetNewAddress()
			if err != nil {
				log.Fatalf("Cannot create address #%d: %v", i+1, err)
			}
			addressSpace[i] = addr
		}(i)
	}
	a.wg.Wait()

	// Start sending funds to the addresses the wallet already owns.
	// At this point we are just going to iterate over our address slice
	// without any use of concurrent primitives. Most of the following code
	// should change once we move to use of many actors.
	balance, err := a.client.GetBalanceMinConf("", 0)
	if err != nil {
		log.Fatalf("Cannot see account balance: %v", err)
	}
	if balance <= 0 {
		log.Println("Insufficient funds! Exiting...")
		return nil
	}

	// Tx per second
	for _ = range time.Tick(time.Second) {
		for i := 0; i < txPerSec; i++ {
			a.client.SendFromMinConf("", addressSpace[rand.Int()%addressNum], balance/10, 0)
		}
	}

	return nil
}

// Stop kills the Actor's wallet process and shuts down any goroutines running
// to manage the Actor's behavior.
func (a *Actor) Stop() (err error) {
	if killErr := a.cmd.Process.Kill(); killErr != nil {
		err = killErr
	}
	close(a.quit)
	a.wg.Wait()
	return
}

// Cleanup removes the directory an Actor's wallet process was previously using.
func (a *Actor) Cleanup() error {
	return os.RemoveAll(a.args.dir)
}

type procArgs struct {
	chainSvr         ChainServer
	dir              string
	port             string
	rpcUser          string
	rpcPass          string
	walletPassphrase string
}

func (p *procArgs) Cmd() *exec.Cmd {
	return exec.Command("btcwallet", p.args()...)
}

func (p *procArgs) args() []string {
	return []string{
		"--simnet",
		"--datadir=" + p.dir,
		"--username=" + p.rpcUser,
		"--password=" + p.rpcPass,
		"--rpcconnect=" + p.chainSvr.connect,
		"--rpclisten=:" + p.port,
		"--rpccert=" + p.chainSvr.certPath,
		"--rpckey=" + p.chainSvr.keyPath,
	}
}
