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
	wg         sync.WaitGroup
	closed     bool
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
	defer a.wg.Wait()
	// Overwriting the previously created command would be sad.
	if a.cmd != nil {
		return errors.New("actor command previously created")
	}

	// Starting amount at 50 BTC
	amount := btcutil.Amount(50 * btcutil.SatoshiPerBitcoin)
	spendAfter := make(chan struct{})
	start := true
	balanceUpdate := make(chan btcutil.Amount, 1)
	connected := make(chan struct{})
	var firstConn bool
	const timeoutSecs int64 = 3600 * 24

	a.downstream = com.downstream
	a.upstream = com.upstream
	a.stop = com.stop

	// Create and start command in background.
	a.cmd = a.args.Cmd()
	a.cmd.Stdout = stdout
	a.cmd.Stderr = stderr
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
			// Block actors at start until coinbase matures (equals or is more than 50 BTC).
			if balance >= amount && start {
				spendAfter <- struct{}{}
				start = false
			}
			if balance != 0 && len(balanceUpdate) == 0 {
				balanceUpdate <- balance
			} else if balance != 0 {
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
		a.Shutdown()
		return connErr
	}

	// Wait for btcd to connect
	<-connected

	// Create the wallet.
	if err := a.client.CreateEncryptedWallet(a.args.walletPassphrase); err != nil {
		a.Shutdown()
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
		return err
	}

	// Send a random address upstream that will be used by the cpu miner.
	a.upstream <- addressSpace[rand.Int()%a.addressNum]

	select {
	// Wait for matured coinbase
	case <-spendAfter:
		log.Println("Start spending funds")
	case <-a.quit:
		// If the simulation ends for any reason before the actor's coinbase
		// matures, we don't want it to get stuck on spendAfter.
		return nil
	}

	balance := <-balanceUpdate

	// Start a goroutine to send addresses upstream.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			select {
			case a.upstream <- addressSpace[rand.Int()%a.addressNum]:
				// Send address to upstream to request receiving a transaction.
			case <-a.quit:
				return
			}
		}
	}()

	// rpcSync is used for synchronization, so the last call to SendFromMinConf
	// will happen before the call to ListTransactions.
	rpcSync := make(chan struct{}, 1)

	// Start a goroutine to send transactions.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			select {
			case addr := <-a.downstream:
				// Receive address from downstream to send a transaction to.
				if _, err := a.client.SendFromMinConf("", addr, amount, 0); err != nil {
					log.Printf("Cannot send transaction: %v", err)
				}
			case <-a.quit:
				rpcSync <- struct{}{}
				return
			}
		}
	}()

out:
	// Receive account balance updates.
	for {
		select {
		case balance = <-balanceUpdate:
			// Start sending funds
			if balance >= amount && a.downstream == nil {
				a.downstream = com.downstream
				// else block downstream (ie. stop spending funds)
			} else if balance < amount && a.downstream != nil {
				a.downstream = nil
			}
		case <-a.quit:
			break out
		}
	}

	<-rpcSync
	txCount := 50
	txn, err := a.client.ListTransactionsCount("", txCount)
	if err != nil {
		log.Printf("Actor on %s cannot list transactions: %v", rpcConf.Host, err)
		return nil
	}
	if len(txn) == 0 {
		log.Printf("Actor on %s doesn't have any transactions", rpcConf.Host)
		return nil
	}
	com.txChan <- txn

	return nil
}

// Stop closes the client and quit channel so every running goroutine
// can return or just exits if quit has already been closed.
func (a *Actor) Stop() {
	select {
	case <-a.quit:
	default:
		close(a.quit)
	}
}

// Shutdown stops the rpc client, kills the actor btcwallet process
// and removes its data directories.
func (a *Actor) Shutdown() {
	if !a.closed {
		a.client.Shutdown()
		log.Printf("Actor on %s shutdown successfully", "localhost:"+a.args.port)
		if err := Exit(a.cmd); err != nil {
			log.Printf("Cannot exit actor on %s: %v", "localhost:"+a.args.port, err)
		}
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot cleanup actor directory on %s: %v", "localhost:"+a.args.port, err)
		}
		a.closed = true
	} else {
		log.Printf("Actor on %s already shutdown", "localhost:"+a.args.port)
	}
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
		"-d" + "TXST=warn",
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
