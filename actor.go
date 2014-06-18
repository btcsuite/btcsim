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
	"time"

	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
)

// Actor describes an actor on the simulation network.  Each actor runs
// independantly without external input to decide it's behavior.
type Actor struct {
	args       procArgs
	cmd        *exec.Cmd
	client     *rpc.Client
	addressNum int
	downstream chan btcutil.Address
	upstream   chan btcutil.Address
	stop       chan struct{}
	quit       chan struct{}
}

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
			walletPassphrase: "walletpass",
		},
		addressNum: 1000,
		quit:       make(chan struct{}),
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
func (a *Actor) Start(stderr, stdout io.Writer, com Communication) error {
	// Overwriting the previously created command would be sad.
	if a.cmd != nil {
		return errors.New("actor command previously created")
	}

	balanceUpdate := make(chan btcutil.Amount, 1)
	connected := make(chan struct{})
	var firstConn bool
	const timeoutSecs int64 = 3600 * 24

	a.downstream = com.downstream
	a.upstream = com.upstream
	a.stop = com.stop

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
		Endpoint:     "ws",
		User:         a.args.chainSvr.user,
		Pass:         a.args.chainSvr.pass,
		Certificates: a.args.chainSvr.cert,
	}

	ntfnHandlers := rpc.NotificationHandlers{
		OnBtcdConnected: func(conn bool) {
			if conn && !firstConn {
				firstConn = true
				connected <- struct{}{}
			}
		},
		// Update on every round bitcoin value.
		OnAccountBalance: func(account string, balance btcutil.Amount, confirmed bool) {
			if balance%1 == 0 && len(balanceUpdate) == 0 {
				balanceUpdate <- balance
			} else if balance%1 == 0 {
				// Discard previous update
				<-balanceUpdate
				balanceUpdate <- balance
			}
		},
	}

	// The RPC client will not wait for the RPC server to start up, so
	// loop a few times and attempt additional connections, sleeping
	// after each failure.
	var client *rpc.Client
	var connErr error
	for i := 0; i < connRetry; i++ {
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

	// Wait for btcd to connect
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

	// Create wallet addresses and unlock wallet.
	log.Printf("%s: Creating wallet addresses. This may take a while...", rpcConf.Host)
	addressSpace := make([]btcutil.Address, a.addressNum)
	for i := range addressSpace {
		addr, err := client.GetNewAddress()
		if err != nil {
			log.Printf("%s: Cannot create address #%d", rpcConf.Host, i+1)
			return err
		}
		addressSpace[i] = addr
	}
	if err := a.client.WalletPassphrase(a.args.walletPassphrase, timeoutSecs); err != nil {
		log.Printf("%s: Cannot unlock wallet: %v", rpcConf.Host, err)
		return nil
	}

	// TODO: Probably add OnRescanFinished notification and make it sync here

	// Send a random address upstream that will be used by the cpu miner.
	a.upstream <- addressSpace[rand.Int()%a.addressNum]

	// Receive from downstream (ie. start spending funds) only after coinbase matures
	a.downstream = nil
	var balance btcutil.Amount

out:
	for {
		select {
		case a.upstream <- addressSpace[rand.Int()%a.addressNum]:
		case addr := <-a.downstream:
			// TODO: Probably handle following error better
			if _, err := a.client.SendFromMinConf("", addr, 1, 0); err != nil {
				log.Printf("%s: Cannot proceed with latest transaction: %v", rpcConf.Host, err)
			}
		case balance = <-balanceUpdate:
			// Start sending funds
			if balance > 100 && a.downstream == nil {
				a.downstream = com.downstream
				// else block downstream (ie. stop spending funds)
			} else if balance == 0 && a.downstream != nil {
				a.downstream = nil
			}
		case <-a.quit:
			break out
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
	a.cmd.Wait()
	close(a.quit)
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
	walletPassphrase string
}

func (p *procArgs) Cmd() *exec.Cmd {
	return exec.Command("btcwallet", p.args()...)
}

func (p *procArgs) args() []string {
	return []string{
		"--simnet",
		"--datadir=" + p.dir,
		"--username=" + p.chainSvr.user,
		"--password=" + p.chainSvr.pass,
		"--rpcconnect=" + p.chainSvr.connect,
		"--rpclisten=:" + p.port,
		"--rpccert=" + p.chainSvr.certPath,
		"--rpckey=" + p.chainSvr.keyPath,
	}
}
