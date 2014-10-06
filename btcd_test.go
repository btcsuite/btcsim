package main

import "testing"

func TestNewBtcdArgs(t *testing.T) {
	prefix := "miner"
	err := genCertPair(CertFile, KeyFile)
	if err != nil {
		t.Errorf("genCertPair error: %v", err)
	}
	args, err := NewBtcdArgs(prefix)
	defer args.Cleanup()
	if err != nil {
		t.Errorf("NewBtcdArgs error: %v", err)
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
		t.Errorf("NewBtcdArgs wrong len expected: %v, got %v", len(expectedArgs.Arguments()), len(args.Arguments()))
	}
	expectedArguments := expectedArgs.Arguments()
	arguments := args.Arguments()
	for i := 0; i < 4; i++ {
		if expectedArguments[i] != arguments[i] {
			t.Errorf("NewBtcdArgs expected: %v, got %v", expectedArguments[i], arguments[i])
		}
	}
	for i := 4; i < len(arguments); i++ {
		if arguments[i] == "" {
			t.Errorf("NewBtcdArgs expected default value, got %v", arguments[i])
		}
	}
}
