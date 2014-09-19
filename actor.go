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

	"github.com/conformal/btcchain"
	"github.com/conformal/btcjson"
	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
)

// minFee is the minimum tx fee that can be paid
const minFee btcutil.Amount = 1e4 // 0.0001 BTC

// Actor describes an actor on the simulation network.  Each actor runs
// independantly without external input to decide it's behavior.
type Actor struct {
	args         procArgs
	cmd          *exec.Cmd
	client       *rpc.Client
	maxAddresses int
	addrs        map[string]struct{}
	downstream   chan TxSplit
	upstream     chan btcutil.Address
	txpool       chan struct{}
	errChan      chan struct{}
	quit         chan struct{}
	coinbase     chan *btcutil.Tx
	wg           sync.WaitGroup
	closed       bool
	utxos        []*TxOut
	enqueueUtxo  chan *TxOut
	dequeueUtxo  chan *TxOut
	change       btcutil.Address
}

// TxOut is a valid tx output that can be used to generate transactions
type TxOut struct {
	OutPoint *btcwire.OutPoint
	Amount   btcutil.Amount
}

// TxSplit includes the address to which a tx should be sent
// and the number of 'splits' to split the change and return it
//
// This is used to build up a large set of utxos that can be used
// to simulate large tx/block ratios
type TxSplit struct {
	address btcutil.Address
	split   int
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
		coinbase:     make(chan *btcutil.Tx, btcchain.CoinbaseMaturity),
		addrs:        make(map[string]struct{}),
		maxAddresses: *maxAddresses,
		quit:         make(chan struct{}),
		enqueueUtxo:  make(chan *TxOut),
		dequeueUtxo:  make(chan *TxOut),
	}
	return &a, nil
}

// Start creates the command to execute a wallet process and starts the
// command in the background, attaching the command's stderr and stdout
// to the passed writers. Nil writers may be used to discard output.
//
// In addition to starting the wallet process, this runs goroutines to
// handle wallet notifications and requests the wallet process to create
// an intial encrypted wallet, so that it can actually send and receive BTC.
//
// If the RPC client connection cannot be established or wallet cannot
// be created, the wallet process is killed and the actor directory
// removed.
func (a *Actor) Start(stderr, stdout io.Writer, com *Communication) error {
	a.downstream = com.downstream
	a.upstream = com.upstream
	a.errChan = com.errChan
	a.txpool = com.txpool

	// Overwriting the previously created command would be sad.
	if a.cmd != nil {
		a.errChan <- struct{}{}
		return errors.New("actor command previously created")
	}

	connected := make(chan struct{})
	var firstConn bool
	const timeoutSecs int64 = 3600 * 24

	// Create and start command in background.
	a.cmd = a.args.Cmd()
	a.cmd.Stdout = stdout
	a.cmd.Stderr = stderr
	if err := a.cmd.Start(); err != nil {
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot remove actor directory after "+
				"failed start of wallet process: %v", err)
		}
		a.errChan <- struct{}{}
		return err
	}

	// Create and start RPC client.
	rpcConf := rpc.ConnConfig{
		Host:                 "localhost:" + a.args.port,
		Endpoint:             "ws",
		User:                 a.args.chainSvr.user,
		Pass:                 a.args.chainSvr.pass,
		Certificates:         a.args.chainSvr.cert,
		DisableAutoReconnect: true,
	}

	ntfnHandlers := rpc.NotificationHandlers{
		OnBtcdConnected: func(conn bool) {
			if conn && !firstConn {
				firstConn = true
				connected <- struct{}{}
			}
		},
	}

	// The RPC client will not wait for the RPC server to start up, so
	// loop a few times and attempt additional connections, sleeping
	// after each failure.
	var client *rpc.Client
	var connErr error
	for i := 0; i < *maxConnRetries; i++ {
		if client, connErr = rpc.New(&rpcConf, &ntfnHandlers); connErr != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		a.client = client
		break
	}
	if a.client == nil {
		a.errChan <- struct{}{}
		return connErr
	}

	// Wait for btcd to connect
	<-connected

	// Create the wallet.
	if err := a.client.CreateEncryptedWallet(a.args.walletPassphrase); err != nil {
		a.errChan <- struct{}{}
		return err
	}

	// Wait for wallet sync
	for i := 0; i < *maxConnRetries; i++ {
		if _, err := a.client.GetBalance(""); err != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		break
	}

	// Create wallet addresses and unlock wallet.
	log.Printf("%s: Creating wallet addresses. This may take a while...", rpcConf.Host)
	addressSpace := make([]btcutil.Address, a.maxAddresses)
	for i := range addressSpace {
		addr, err := a.client.GetNewAddress()
		if err != nil {
			log.Printf("%s: Cannot create address #%d", rpcConf.Host, i+1)
			a.errChan <- struct{}{}
			return err
		}
		addressSpace[i] = addr
		a.addrs[addr.String()] = struct{}{}
	}

	if err := a.client.WalletPassphrase(a.args.walletPassphrase, timeoutSecs); err != nil {
		log.Printf("%s: Cannot unlock wallet: %v", rpcConf.Host, err)
		a.errChan <- struct{}{}
		return err
	}

	// Send a random address upstream that will be used by the cpu miner.
	a.upstream <- addressSpace[rand.Int()%a.maxAddresses]

	// Start a goroutine to send addresses upstream.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		for {
			select {
			case a.upstream <- addressSpace[rand.Int()%a.maxAddresses]:
				// Send address to upstream to request receiving a transaction.
			case <-a.quit:
				return
			}
		}
	}()

	// Start a goroutine that queues up a set of utxos belonging to this
	// actor. The utxos are sent from com.poolUtxos which in turn receives
	// block notifications from sim.go
	a.wg.Add(1)
	go a.queueUtxos()

	// Start a goroutine to send transactions.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		for {
			select {
			case utxo := <-a.dequeueUtxo:
				select {
				case txsplit := <-a.downstream:
					// Create a raw transaction
					txid := utxo.OutPoint.Hash.String()
					inputs := []btcjson.TransactionInput{{txid, utxo.OutPoint.Index}}

					// Provide a fees of minFee to ensure the tx gets mined
					amt := utxo.Amount - minFee
					amounts := map[btcutil.Address]btcutil.Amount{}

					// For each 'split', create a output of random amount and sent it
					// to a random address from the address space
					for i := 0; i < txsplit.split; i++ {
						// skip if this amt can't be split any further
						if amt > minFee {
							// pick a random address
							to := addressSpace[rand.Int()%a.maxAddresses]
							// pick a random change amount which is less than amt
							// but have a lower bound at minFee
							change := btcutil.Amount(rand.Int63n(int64(amt) / 2))
							if change < minFee {
								change = minFee
							}
							amounts[to] = change
							amt -= change
						}
					}
					// Send the remaining amount to the address received downstream
					amounts[txsplit.address] = amt

					msgTx, err := a.client.CreateRawTransaction(inputs, amounts)
					if err != nil {
						log.Printf("%s: Cannot create raw transaction: %v", rpcConf.Host, err)
						if a.txpool != nil {
							a.txpool <- struct{}{}
						}
						continue
					}
					// sign it
					msgTx, ok, err := a.client.SignRawTransaction(msgTx)
					if err != nil {
						log.Printf("%s: Cannot sign raw transaction: %v", rpcConf.Host, err)
						if a.txpool != nil {
							a.txpool <- struct{}{}
						}
						continue
					}
					if !ok {
						log.Printf("%s: Not all inputs have been signed", rpcConf.Host)
						if a.txpool != nil {
							a.txpool <- struct{}{}
						}
						continue
					}
					// and finally send it.
					if _, err := a.client.SendRawTransaction(msgTx, false); err != nil {
						log.Printf("%s: Cannot send raw transaction: %v", rpcConf.Host, err)
						if a.txpool != nil {
							a.txpool <- struct{}{}
						}
						continue
					}
				case <-a.quit:
					return
				}
			case <-a.quit:
				return
			}
		}
	}()

	return nil
}

// queueUtxos receives utxos belonging to this actor and queues them up
func (a *Actor) queueUtxos() {
	defer a.wg.Done()

	enqueue := a.enqueueUtxo
	var dequeue chan *TxOut
	var next *TxOut
out:
	for {
		select {
		case n, ok := <-enqueue:
			if !ok {
				// If no a.utxos are queued for handling,
				// the queue is finished.
				if len(a.utxos) == 0 {
					break out
				}
				// nil channel so no more reads can occur.
				enqueue = nil
				continue
			}
			if len(a.utxos) == 0 {
				next = n
				dequeue = a.dequeueUtxo
			}
			a.utxos = append(a.utxos, n)
		case dequeue <- next:
			a.utxos[0] = nil
			a.utxos = a.utxos[1:]
			if len(a.utxos) != 0 {
				next = a.utxos[0]
			} else {
				// If no more a.utxos can be enqueued, the
				// queue is finished.
				if enqueue == nil {
					break out
				}
				dequeue = nil
			}
		case <-a.quit:
			break out
		}
	}
	close(a.dequeueUtxo)
}

// Stop closes the client and quit channel so every running goroutine
// can return or just exits if quit has already been closed.
func (a *Actor) Stop() {
	select {
	case <-a.quit:
	default:
		go a.drainChans()
		a.client.Shutdown()
		close(a.quit)
	}
}

// WaitForShutdown waits until every actor goroutine has returned
func (a *Actor) WaitForShutdown() {
	a.wg.Wait()
}

// Shutdown kills the actor btcwallet process and removes its data directories.
func (a *Actor) Shutdown() {
	if !a.closed {
		a.client.Shutdown()
		if err := Exit(a.cmd); err != nil {
			log.Printf("Cannot exit actor on %s: %v", "localhost:"+a.args.port, err)
		}
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot cleanup actor directory on %s: %v", "localhost:"+a.args.port, err)
		}
		a.closed = true
		log.Printf("Actor on %s shutdown successfully", "localhost:"+a.args.port)
	} else {
		log.Printf("Actor on %s already shutdown", "localhost:"+a.args.port)
	}
}

// drainChans drains all chans that might block an actor from shutting down
func (a *Actor) drainChans() {
	for {
		select {
		case <-a.downstream:
		case <-a.upstream:
		case <-a.txpool:
		case <-a.dequeueUtxo:
		case <-a.errChan:
		}
	}
}

// Cleanup removes the directory an Actor's wallet process was previously using.
func (a *Actor) Cleanup() error {
	return os.RemoveAll(a.args.dir)
}

// ForceShutdown shutdowns an actor that unexpectedly exited a.Start
func (a *Actor) ForceShutdown() {
	a.Stop()
	a.WaitForShutdown()
	a.Shutdown()
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
