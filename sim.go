// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
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

// For now, hardcode a single already-running btcd connection that is used for
// each actor. This should be changed to start a new btcd with the --simnet
// flag, and each actor can connect to the spawned btcd process.
var defaultChainServer = ChainServer{
	connect: "localhost:18556", // local simnet btcd
	user:    "rpcuser",
	pass:    "rpcpass",
}

var (
	// maxConnRetries defines the number of times to retry rpc client connections
	maxConnRetries = flag.Int("maxconnretries", 15, "Maximum retries to connect to rpc client")

	// maxActors defines the Number of actors to spawn
	maxActors = flag.Int("maxactors", 1, "Maximum number of actors")

	// maxBlocks defines how many blocks have to connect to the blockchain
	// before the simulation normally stops
	maxBlocks = flag.Int("maxblocks", 20000, "Maximum blocks to generate")

	// matureBlock defines after which block the blockchain is mature enough to start
	// controlled mining as per the tx curve
	matureBlock = flag.Int("matureblock", 20000, "Block number at blockchain maturity")

	// maxAddresses defines the number of addresses to generate per actor
	maxAddresses = flag.Int("maxaddresses", 100, "Maximum addresses per actor")

	// maxBlockSize defines the maximum block size to be passed as -blockmaxsize to the miner
	maxBlockSize = flag.Int("maxblocksize", 999000, "Maximum block size used by the miner")

	// txCurvePath is the path to a CSV file containing the block, utxo count, tx count
	txCurvePath = flag.String("txcurve", "",
		"Path to the CSV File containing block, utxo count, tx count fields")

	// tpb is transactions per block that will be used to generate a csv file
	// containing <block #>, <txCount> fields
	tpb = flag.Int("tpb", 100, "Transactions per block")
)

func init() {
	flag.Parse()

	runtime.GOMAXPROCS(runtime.NumCPU())
	rand.Seed(time.Now().UnixNano())
}

func main() {
	if *txCurvePath == "" {
		fmt.Println("Usage: btcsim -txcurve {.csv} [{other flags}]")
		flag.PrintDefaults()
		os.Exit(0)
	}
	// txCurve is a slice of Rows, each corresponding
	// to a row in the input CSV file
	// if txCurve is not nil, we control mining so as to
	// get the same block vs tx count as the input curve
	var txCurve map[int32]*Row
	if *txCurvePath != "" {
		var err error
		txCurve, err = readCSV(*txCurvePath)
		if err != nil {
			log.Fatalf("Error reading tx curve CSV: %v", err)
			return
		}
	}
	// set min block number from the curve as matureBlock
	for k := range txCurve {
		block := int(k)
		if block < *matureBlock {
			*matureBlock = block
		}
	}

	actors := make([]*Actor, 0, *maxActors)
	com := NewCommunication()
	// we need only enough blocks after matureBlock
	// to generate the tx curve
	*maxBlocks = *matureBlock + len(txCurve)

	btcdHomeDir := btcutil.AppDataDir("btcd", false)
	defaultChainServer.certPath = filepath.Join(btcdHomeDir, "rpc.cert")
	defaultChainServer.keyPath = filepath.Join(btcdHomeDir, "rpc.key")
	cert, err := ioutil.ReadFile(defaultChainServer.certPath)
	if err != nil {
		log.Fatalf("Cannot read certificate: %v", err)
	}
	defaultChainServer.cert = cert

	datadir, err := ioutil.TempDir("", "chainServerData")
	if err != nil {
		log.Fatalf("Cannot read certificate: %v", err)
	}
	defer func(datadir string) {
		if err := os.RemoveAll(datadir); err != nil {
			log.Printf("Cannot remove mining btcd datadir: %v", err)
			return
		}
	}(datadir)

	btcdArgs := []string{
		"--simnet",
		"-u" + defaultChainServer.user,
		"-P" + defaultChainServer.pass,
		"--datadir=" + datadir,
		"--rpccert=" + defaultChainServer.certPath,
		"--rpckey=" + defaultChainServer.keyPath,
		"--profile=",
	}

	log.Println("Starting btcd on simnet...")
	btcd := exec.Command("btcd", btcdArgs...)
	if err := btcd.Start(); err != nil {
		log.Fatalf("Couldn't start btcd: %v", err)
	}

	// Create and start RPC client.
	rpcConf := rpc.ConnConfig{
		Host:                 defaultChainServer.connect,
		Endpoint:             "ws",
		User:                 defaultChainServer.user,
		Pass:                 defaultChainServer.pass,
		Certificates:         defaultChainServer.cert,
		DisableAutoReconnect: true,
	}

	ntfnHandlers := rpc.NotificationHandlers{
		OnBlockConnected: func(hash *btcwire.ShaHash, height int32) {
			block := &Block{
				hash:   hash,
				height: height,
			}
			select {
			case com.enqueueBlock <- block:
			case <-com.exit:
			}
		},
		OnTxAccepted: func(hash *btcwire.ShaHash, amount btcutil.Amount) {
			com.timeReceived <- time.Now()
		},
	}

	var client *rpc.Client
	for i := 0; i < *maxConnRetries; i++ {
		if client, err = rpc.New(&rpcConf, &ntfnHandlers); err != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		break
	}
	if client == nil {
		log.Printf("Cannot start btcd rpc client: %v", err)
		if err := Exit(btcd); err != nil {
			log.Printf("Cannot kill initial btcd process: %v", err)
		}
		return
	}

	// Register for block notifications.
	if err := client.NotifyBlocks(); err != nil {
		log.Printf("Cannot register for block notifications: %v", err)
		return
	}

	// Register for transaction notifications
	if err := client.NotifyNewTransactions(false); err != nil {
		log.Printf("Cannot register for transactions notifications: %v", err)
		if err := Exit(btcd); err != nil {
			log.Printf("Cannot kill initial btcd process: %v", err)
		}
		return
	}

	for i := 0; i < *maxActors; i++ {
		a, err := NewActor(&defaultChainServer, uint16(18557+i))
		if err != nil {
			log.Printf("Cannot create actor on %s: %v", "localhost:"+a.args.port, err)
			continue
		}
		actors = append(actors, a)
	}

	// if we receive an interrupt, proceed to shutdown
	addInterruptHandler(func() {
		safeClose(com.exit)
	})

	// Start simulation.
	tpsChan, tpbChan := com.Start(actors, client, btcd, txCurve)
	com.WaitForShutdown()

	tps, ok := <-tpsChan
	if ok && !math.IsNaN(tps) {
		log.Printf("Average transactions per sec: %.2f", tps)
	}

	tpb, ok := <-tpbChan
	if ok && tpb > 0 {
		log.Printf("Maximum transactions per block: %v", tpb)
	}
}

// Exit closes the cmd by passing SIGINT
// workaround for windows by passing SIGKILL
func Exit(cmd *exec.Cmd) (err error) {
	defer cmd.Wait()

	if runtime.GOOS == "windows" {
		err = cmd.Process.Signal(os.Kill)
	} else {
		err = cmd.Process.Signal(os.Interrupt)
	}

	return
}

// Close sends close signal to actors, waits for actor goroutines
// to exit and then shuts down all actors.
func Close(actors []*Actor) {
	// Stop actors by shuting down their rpc client and closing quit channel.
	for _, a := range actors {
		a.WaitForShutdown()
	}

	// shutdown only after all actors have stopped
	for _, a := range actors {
		a.Shutdown()
	}
}

// safeClose safely closes the exit channel.
func safeClose(exit chan struct{}) {
	select {
	case <-exit:
		return
	default:
	}
	close(exit)
}
