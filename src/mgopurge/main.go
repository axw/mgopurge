// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package main

import (
	"bufio"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	jujutxn "github.com/juju/txn"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

const txnsC = "txns"
const txnsStashC = txnsC + ".stash"
const machinesC = "machines"

var controllerPrompt = `
This program should only be used to recover from specific transaction
related problems in a Juju database. Casual use is strongly
discouraged. Irreversible damage may be caused to a Juju deployment
through improper use of this tool.

This program should not be run while any Juju controller machine
agents are running.

Have all controller machine agents been shut down?`[1:]

func main() {
	checkErr("setupLogging", setupLogging())
	args := commandLine()

	if args.doPrompt && !promptYN(controllerPrompt) {
		return
	}

	session, err := dial(args)
	checkErr("Dial", err)

	db := session.DB("juju")
	collections := getAllPurgeableCollections(db)
	txns := db.C(txnsC)

	logger.Infof("Repairing runaway transactions for apiHostPorts document...")
	err = FixApiHostPorts(db, txns)
	checkErr("FixApiHostPorts", err)

	logger.Infof("Purging orphaned transactions for %d juju collections...\n", len(collections))
	err = PurgeMissing(txns, db.C(txnsStashC), collections...)
	checkErr("PurgeMissing", err)
	logger.Infof("Done compacting orphaned transactions.")

	if args.doMachines {
		logger.Infof("Removing references to completed transactions in machines collection...")
		err = FixMachinesTxnQueue(db.C(machinesC), txns)
		checkErr("FixMachinesTxnQueue", err)
	}

	if args.doPrune {
		logger.Infof("Pruning unreferenced transactions...")
		err = jujutxn.PruneTxns(db, txns)
		checkErr("PruneTxns", err)
	}

	if args.doCompcact {
		logger.Infof("Compacting database to release disk space...")
		err = db.Run(bson.M{"repairDatabase": 1}, nil)
		checkErr("repairDatabase", err)
	}
}

func promptYN(question string) bool {
	fmt.Printf("%s [y/n] ", question)
	os.Stdout.Sync()
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	switch strings.ToLower(scanner.Text()) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func printAndFlush(s string) {
	fmt.Print(s)
	os.Stdout.Sync()
}

func dial(args commandLineArgs) (*mgo.Session, error) {
	info := &mgo.DialInfo{
		Addrs: []string{net.JoinHostPort(args.hostname, args.port)},
	}
	if args.username != "" {
		info.Database = "admin"
		info.Username = args.username
		info.Password = args.password
	}
	if args.ssl {
		info.DialServer = dialSSL
	}
	session, err := mgo.DialWithInfo(info)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func dialSSL(addr *mgo.ServerAddr) (net.Conn, error) {
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
	}
	cc := tls.Client(c, tlsConfig)
	if err := cc.Handshake(); err != nil {
		return nil, err
	}
	return cc, nil
}

func checkErr(label string, err error) {
	if err != nil {
		logger.Errorf("%s: %s", label, err)
		os.Exit(1)
	}
}

type commandLineArgs struct {
	hostname   string
	port       string
	ssl        bool
	username   string
	password   string
	doPrompt   bool
	doMachines bool
	doPrune    bool
	doCompcact bool
}

func commandLine() commandLineArgs {
	flags := flag.NewFlagSet("mgopurge", flag.ExitOnError)
	var a commandLineArgs
	flags.StringVar(&a.hostname, "hostname", "localhost",
		"hostname of the Juju MongoDB server")
	flags.StringVar(&a.port, "port", "37017",
		"port of the Juju MongoDB server")
	flags.BoolVar(&a.ssl, "ssl", true,
		"use SSL to connect to MonogDB ")
	flags.StringVar(&a.username, "username", "admin",
		"user for connecting to MonogDB (use \"\" to for no authentication)")
	flags.StringVar(&a.password, "password", "",
		"password for connecting to MonogDB")
	yes := flags.Bool("yes", false, "answer 'yes' to prompts")
	noMachines := flags.Bool("no-machines", false,
		"skip removal of completed txn-queue entries from machines collection")
	noPrune := flags.Bool("no-prune", false,
		"skip pruning of completed transactions")
	noCompact := flags.Bool("no-compact", false,
		"skip compacting of database")

	flags.Parse(os.Args[1:])

	if a.password == "" && a.username != "" {
		fmt.Fprintf(os.Stderr, "error: -password must be used if username is provided\n")
		os.Exit(2)
	}
	a.doPrompt = !*yes
	a.doMachines = !*noMachines
	a.doPrune = !*noPrune
	a.doCompcact = !*noCompact
	return a
}

func isPurgeableCollection(name string) bool {
	if name == txnsC {
		return false
	}
	if strings.HasPrefix(name, "system.") {
		return false
	}
	if strings.HasPrefix(name, txnsC+".") {
		return false
	}
	return true
}

func getAllPurgeableCollections(db *mgo.Database) (collections []string) {
	allCollections, err := db.CollectionNames()
	checkErr("CollectionNames", err)
	for _, collection := range allCollections {
		if isPurgeableCollection(collection) {
			collections = append(collections, collection)
		}
	}
	return
}
