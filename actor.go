// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
)

// ChainServer describes the arguments necessary to connect a btcwallet
// instance to a btcd websocket RPC server.
type ChainServer struct {
	connect string
	user    string
	pass    string
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
	actorRPCUser          = "potato"
	actorRPCPass          = "carrot"
)

// NewActor creates a new actor which runs its own wallet process connecting
// to the btcd chain server specified by chain, and listening for simulator
// websocket connections on the specified port.
func NewActor(chain *ChainServer, port uint16) (*Actor, error) {
	// Please don't run this as root.
	if port < 1024 {
		return nil, errors.New("invalid actor port")
	}

	// TODO: probably a better idea to write a single keypair and each
	// actor (and btcd) can reference it with the --rpccert and --rpckey
	// options.  Switch to that when we start spawning our own simnet
	// btcd instances.
	validUntil := time.Now().Add(10 * 365 * 24 * time.Hour)
	cert, key, err := btcutil.NewTLSCertPair("actor", validUntil, nil)
	if err != nil {
		return nil, err
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
			rpcCert:          cert,
			rpcKey:           key,
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

	if err := a.writeCertPair(); err != nil {
		return err
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
		Certificates: a.args.rpcCert,
	}
	// The RPC client will not wait for the RPC server to start up, so
	// loop a few times and attempt additional connections, sleeping
	// after each failure.
	var client *rpc.Client
	var connErr error
	for i := 0; i < 5; i++ {
		if client, connErr = rpc.New(&rpcConf, nil); connErr != nil {
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

	// Create the wallet.
	err := a.client.CreateEncryptedWallet(a.args.walletPassphrase)
	if err != nil {
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

	// Start actor behavior goroutines.
	a.wg.Add(0) // Increment for each goroutine run

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

// writeCertPair writes the TLS certificate and key to the wallet process's
// home directory.  It is necessary to generate certificates here rather than
// letting the wallet auto generate them to prevent a race where we can't
// establish an rpc client connection due to missing a missing cert file.
//
// It's assumed the actor directory already exists at this point (due to a call
// to ioutil.TempDir).
func (a *Actor) writeCertPair() error {
	certPath := filepath.Join(a.args.dir, "rpc.cert")
	keyPath := filepath.Join(a.args.dir, "rpc.key")

	err := ioutil.WriteFile(certPath, a.args.rpcCert, 0666)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(keyPath, a.args.rpcKey, 0600)
}

type procArgs struct {
	chainSvr         ChainServer
	dir              string
	port             string
	rpcCert          []byte
	rpcKey           []byte
	rpcUser          string
	rpcPass          string
	walletPassphrase string
}

func (p *procArgs) Cmd() *exec.Cmd {
	return exec.Command("btcwallet", p.args()...)
}

func (p *procArgs) args() []string {
	return []string{
		"--datadir=" + p.dir,
		"--username=" + p.rpcUser,
		"--password=" + p.rpcPass,
		"--rpcconnect=" + p.chainSvr.connect,
		"--btcdusername=" + p.chainSvr.user,
		"--btcdpassword=" + p.chainSvr.pass,
		"--rpclisten=:" + p.port,
	}
}
