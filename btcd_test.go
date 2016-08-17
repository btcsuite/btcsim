// Copyright (c) 2014-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import "testing"

func TestNewBtcdArgs(t *testing.T) {
	prefix := "miner"
	err := genCertPair(CertFile, KeyFile)
	if err != nil {
		t.Errorf("genCertPair error: %v", err)
	}
	args, err := newBtcdArgs(prefix)
	defer args.Cleanup()
	if err != nil {
		t.Errorf("newBtcdArgs error: %v", err)
	}
	expectedArgs := &btcdArgs{
		// fixed
		Listen:    "127.0.0.1:18555",
		RPCListen: "127.0.0.1:18556",
		RPCUser:   "user",
		RPCPass:   "pass",
		// the rest are env-dependent and variable
		// don't test these literally
		DataDir: "/tmp/user/1000/miner-data948809262",
		LogDir:  "/tmp/user/1000/miner-logs649955253",
	}
	if len(expectedArgs.Arguments()) != len(args.Arguments()) {
		t.Errorf("newBtcdArgs wrong len expected: %v, got %v", len(expectedArgs.Arguments()), len(args.Arguments()))
	}
	expectedArguments := expectedArgs.Arguments()
	arguments := args.Arguments()
	for i := 0; i < 4; i++ {
		if expectedArguments[i] != arguments[i] {
			t.Errorf("newBtcdArgs expected: %v, got %v", expectedArguments[i], arguments[i])
		}
	}
	for i := 4; i < len(arguments); i++ {
		if arguments[i] == "" {
			t.Errorf("newBtcdArgs expected default value, got %v", arguments[i])
		}
	}
}
