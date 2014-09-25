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

	"github.com/conformal/btcjson"
	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
)

// minFee is the minimum tx fee that can be paid
const minFee btcutil.Amount = 1e4 // 0.0001 BTC

// utxoQueue is the queue of utxos belonging to a actor
// utxos are queued after a block is received and are dispatched
// to their respective owner from com.poolUtxos
// they are dequeued from simulateTx and splitUtxos
type utxoQueue struct {
	utxos   []*TxOut
	enqueue chan *TxOut
	dequeue chan *TxOut
}

// Actor describes an actor on the simulation network.  Each actor runs
// independantly without external input to decide it's behavior.
type Actor struct {
	args           procArgs
	cmd            *exec.Cmd
	client         *rpc.Client
	quit           chan struct{}
	wg             sync.WaitGroup
	ownedAddresses []btcutil.Address
	utxoQueue      *utxoQueue
	miningAddr     chan btcutil.Address
}

// TxOut is a valid tx output that can be used to generate transactions
type TxOut struct {
	OutPoint *btcwire.OutPoint
	Amount   btcutil.Amount
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
		quit:           make(chan struct{}),
		ownedAddresses: make([]btcutil.Address, *maxAddresses),
		miningAddr:     make(chan btcutil.Address),
		utxoQueue: &utxoQueue{
			enqueue: make(chan *TxOut),
			dequeue: make(chan *TxOut),
		},
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
	// Overwriting the previously created command would be sad.
	if a.cmd != nil {
		com.errChan <- struct{}{}
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
		com.errChan <- struct{}{}
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
		com.errChan <- struct{}{}
		return connErr
	}

	// Wait for btcd to connect
	<-connected

	// Create the wallet.
	if err := a.client.CreateEncryptedWallet(a.args.walletPassphrase); err != nil {
		com.errChan <- struct{}{}
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
	for i := range a.ownedAddresses {
		addr, err := a.client.GetNewAddress()
		if err != nil {
			log.Printf("%s: Cannot create address #%d", rpcConf.Host, i+1)
			com.errChan <- struct{}{}
			return err
		}
		a.ownedAddresses[i] = addr
	}

	if err := a.client.WalletPassphrase(a.args.walletPassphrase, timeoutSecs); err != nil {
		log.Printf("%s: Cannot unlock wallet: %v", rpcConf.Host, err)
		com.errChan <- struct{}{}
		return err
	}

	// Send a random address that will be used by the cpu miner.
	a.miningAddr <- a.ownedAddresses[rand.Int()%len(a.ownedAddresses)]

	// Start a goroutine that queues up a set of utxos belonging to this
	// actor. The utxos are sent from com.poolUtxos which in turn receives
	// block notifications from sim.go
	a.wg.Add(1)
	go a.queueUtxos()

	// Start a goroutine to simulate transactions.
	a.wg.Add(1)
	go a.simulateTx(com.downstream, com.txpool)

	// Start a goroutine to split utxos
	a.wg.Add(1)
	go a.splitUtxos(com.split, com.txpool)

	return nil
}

// simulateTx runs as a goroutine and simulates transactions between actors
//
// It receives a random address downstream, dequeues a utxo, sends a raw
// transaction to the address using the utxo as input
func (a *Actor) simulateTx(downstream <-chan btcutil.Address, txpool chan<- struct{}) {
	defer a.wg.Done()

	for {
		select {
		case utxo := <-a.utxoQueue.dequeue:
			select {
			case addr := <-downstream:
				// Create a raw transaction
				inputs := []btcjson.TransactionInput{{
					Txid: utxo.OutPoint.Hash.String(),
					Vout: utxo.OutPoint.Index,
				}}

				// Provide a fees of minFee to ensure the tx gets mined
				amt := utxo.Amount - minFee
				amounts := map[btcutil.Address]btcutil.Amount{
					addr: amt,
				}

				err := a.sendRawTransaction(inputs, amounts)
				if err != nil {
					log.Printf("error sending raw transaction: %v", err)
					select {
					case txpool <- struct{}{}:
					case <-a.quit:
						return
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
}

// splitUtxos runs as a goroutine and builds up a large set of utxos that
// can be used to simulate large tx/block ratios
//
// It receives a 'split' which is int that indicates the number of resultant utxos
// the tx is sent to addresses from the same actor since we're only interested in
// building up the utxo set
func (a *Actor) splitUtxos(split <-chan int, txpool chan<- struct{}) {
	defer a.wg.Done()

	for {
		select {
		case utxo := <-a.utxoQueue.dequeue:
			select {
			case split := <-split:
				// Create a raw transaction
				inputs := []btcjson.TransactionInput{{
					Txid: utxo.OutPoint.Hash.String(),
					Vout: utxo.OutPoint.Index,
				}}

				// Provide a fees of minFee to ensure the tx gets mined
				amt := utxo.Amount - minFee
				amounts := map[btcutil.Address]btcutil.Amount{}

				// Create a output of random amount and sent it
				// to a random address from the address space
				// Iteration is split+1 times taking into account this utxo
				// which is consumed in the process
				for i := 0; i < split+1; i++ {
					// skip if this amt can't be split any further
					if amt > minFee {
						// pick a random address
						to := a.ownedAddresses[rand.Int()%len(a.ownedAddresses)]
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

				err := a.sendRawTransaction(inputs, amounts)
				if err != nil {
					log.Printf("error sending raw transaction: %v", err)
					select {
					case txpool <- struct{}{}:
					case <-a.quit:
						return
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
}

// sendRawTransaction creates a raw transaction, signs it and sends it
func (a *Actor) sendRawTransaction(inputs []btcjson.TransactionInput, amounts map[btcutil.Address]btcutil.Amount) error {
	msgTx, err := a.client.CreateRawTransaction(inputs, amounts)
	if err != nil {
		return err
	}
	// sign it
	msgTx, ok, err := a.client.SignRawTransaction(msgTx)
	if err != nil {
		return err
	}
	if !ok {
		return err
	}
	// and finally send it.
	if _, err := a.client.SendRawTransaction(msgTx, false); err != nil {
		return err
	}
	return nil
}

// queueUtxos receives utxos belonging to this actor and queues them up
func (a *Actor) queueUtxos() {
	defer a.wg.Done()

	enqueue := a.utxoQueue.enqueue
	var dequeue chan *TxOut
	var next *TxOut
out:
	for {
		select {
		case n, ok := <-enqueue:
			if !ok {
				// If no utxos are queued for handling,
				// the queue is finished.
				if len(a.utxoQueue.utxos) == 0 {
					break out
				}
				// nil channel so no more reads can occur.
				enqueue = nil
				continue
			}
			if len(a.utxoQueue.utxos) == 0 {
				next = n
				dequeue = a.utxoQueue.dequeue
			}
			a.utxoQueue.utxos = append(a.utxoQueue.utxos, n)
		case dequeue <- next:
			a.utxoQueue.utxos[0] = nil
			a.utxoQueue.utxos = a.utxoQueue.utxos[1:]
			if len(a.utxoQueue.utxos) != 0 {
				next = a.utxoQueue.utxos[0]
			} else {
				// If no more utxos can be enqueued, the
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
	close(a.utxoQueue.dequeue)
}

// Shutdown performs a shutdown down the actor by first signalling
// all goroutines to stop, waiting for them to stop and them cleaning up
func (a *Actor) Shutdown() {
	select {
	case <-a.quit:
	default:
		close(a.quit)
		go a.drainChans()
		a.WaitForShutdown()
		if a.client != nil {
			a.client.Shutdown()
		}
		if err := Exit(a.cmd); err != nil {
			log.Printf("Cannot exit actor on %s: %v", "localhost:"+a.args.port, err)
		}
		if err := a.Cleanup(); err != nil {
			log.Printf("Cannot cleanup actor directory on %s: %v", "localhost:"+a.args.port, err)
		}
		log.Printf("Actor on %s shutdown successfully", "localhost:"+a.args.port)
	}
}

// WaitForShutdown waits until every actor goroutine has returned
func (a *Actor) WaitForShutdown() {
	a.wg.Wait()
}

// drainChans drains all chans that might block an actor from shutting down
func (a *Actor) drainChans() {
	for {
		select {
		case <-a.utxoQueue.dequeue:
		}
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
