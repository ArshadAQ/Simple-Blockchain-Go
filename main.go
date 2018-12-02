package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/davecgh/go-spew/spew"
	golog "github.com/ipfs/go-log"
	libp2p "github.com/libp2p/go-libp2p"
	crypto "github.com/libp2p/go-libp2p-crypto"
	host "github.com/libp2p/go-libp2p-host"
	net "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	ma "github.com/multiformats/go-multiaddr"
	gologging "github.com/whyrusleeping/go-logging"
)

// No.of leading zeros in the hash
const difficulty = 1

// check if some other miner has mined the block before you
var mined = false

// Block contains contains data that will be written to the blockchain
type Block struct {
	Index     int
	Timestamp string
	BPM       int
	Hash      string
	PrevHash  string

	Difficulty int
	Nonce      string
}

// Blockchain is a chain of Blocks
var Blockchain []Block

// To prevent race conditions
var mutex = &sync.Mutex{}

func main() {

	genesisBlock := Block{}
	genesisBlock = Block{0, (time.Now()).String(), 0, calculateHash(genesisBlock), "", difficulty, ""}
	// pretty print in console
	// spew.Dump(genesisBlock)
	Blockchain = append(Blockchain, genesisBlock)

	golog.SetAllLoggers(gologging.INFO)

	// parse options from the command line

	listenF := flag.Int("l", 0, "wait for incoming connections")
	target := flag.String("d", "", "target peer to dial")
	secio := flag.Bool("secio", false, "enable secio")
	seed := flag.Int64("seed", 0, "set random seed for id generation")
	flag.Parse()

	// if no port number is provided for the server to bind
	if *listenF == 0 {
		log.Fatal("Please provide a port to bind on with -l")
	}

	ha, err := makeBasicHost(*listenF, *secio, *seed)

	if err != nil {
		log.Fatal(err)
	}

	// we are only a host, and don't want to be a peer
	if *target == "" {
		log.Println("listening for connections")

		ha.SetStreamHandler("/p2p/1.0.0", handleStream)

		select {}
	} else {
		ha.SetStreamHandler("/p2p/1.0.0", handleStream)

		/*
			Extract target's peer ID from the given multiaddress
			Algorithm:
				1.Get IPFS address from the target
				2. Get PID from IPFS address
				3. Decode PID to get peerID
		*/

		ipfsaddr, err := ma.NewMultiaddr(*target)
		if err != nil {
			log.Fatalln(err)
		}

		pid, err := ipfsaddr.ValueForProtocol(ma.P_IPFS)

		if err != nil {
			log.Fatalln(err)
		}

		peerID, err := peer.IDB58Decode(pid)

		if err != nil {
			log.Fatalln(err)
		}

		/*
			Algorithm to get Peer address from peer ID
				1. We know the IPFS address
				2. Generate multiaddress using the given peerId
				3. Decapsulate from IPFS addr the multiaddress to get the peer address
		*/

		targetPeerAddr, _ := ma.NewMultiaddr(fmt.Sprintf("/ipfs/%s", peer.IDB58Encode(peerID)))
		targetAddr := ipfsaddr.Decapsulate(targetPeerAddr)

		// Add the peer Id and address in the peerStore
		// This is so that libp2p knows how to contact it

		ha.Peerstore().AddAddr(peerID, targetAddr, pstore.PermanentAddrTTL)

		log.Println("opening stream", peerID)

		// found a host - now establishing a stream to the host
		s, err := ha.NewStream(context.Background(), peerID, "/p2p/1.0.0")

		if err != nil {
			log.Fatalln(err)
		}

		rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

		go writeData(rw)
		go readData(rw)

		// Handle sigterm interrupt
		ch := make(chan os.Signal)

		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-ch
			fmt.Println("Received interrupt!")
			// Exit gracefully, without disturbing other nodes
			cleanup(rw, ha, peerID)
			os.Exit(1)
		}()

		// block exiting the program by using an empty select statement
		select {}

	}
}

// create a libp2p host with a random peer ID listening on the given
// multiaddress. It will use secio if secio is true
func makeBasicHost(listenPort int, secio bool, randseed int64) (host.Host, error) {

	var r io.Reader

	// if seed = 0 use real cryptographic randomness
	// else use a deterministic randomness source to make generated keys stay the same
	// across multiple runs
	if randseed == 0 {
		// crypto/rand pkg
		r = rand.Reader
	} else {
		r = mrand.New(mrand.NewSource(randseed))
	}

	// Generate a key pair for this host to obtain a valid host ID later on
	priv, _, err := crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, r)

	// fmt.Println("Private key is: ", priv)
	if err != nil {
		return nil, err
	}

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", listenPort)),
		libp2p.Identity(priv),
	}

	// option removed from golibp2p
	// if !secio {
	// 	opts = append(opts, libp2p.NoEncryption())
	// }
	basicHost, err := libp2p.New(context.Background(), opts...)

	if err != nil {
		return nil, err
	}

	// fmt.Println("Basic host is", basicHost)
	hostAddr, _ := ma.NewMultiaddr(fmt.Sprintf("/ipfs/%s", basicHost.ID().Pretty()))

	// fmt.Println("HostAddr is", hostAddr)
	addrs := basicHost.Addrs()
	var addr ma.Multiaddr
	for _, i := range addrs {
		if strings.HasPrefix(i.String(), "/ip4") {
			addr = i
			break
		}
	}
	// addr := addrs[1]
	fmt.Println("addrs is", addrs)
	fmt.Println("addr is", addr)
	fullAddr := addr.Encapsulate(hostAddr)
	log.Printf("I am %s\n", fullAddr)

	if secio {
		log.Printf("Now run \"go run main.go -l %d -d %s -secio\" on a different terminal \n", listenPort+1, fullAddr)
	} else {
		log.Printf("Now run \"go run main.go -l %d -d %s\" on a different terminal \n", listenPort+1, fullAddr)
	}

	return basicHost, nil
}

// deal with incoming connections that want to overwrite the blockchain
// set up reading input from the connection and
// writing to the connection stream
func handleStream(s net.Stream) {

	log.Println("Got a new stream!")

	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

	go readData(rw)
	go writeData(rw)

	// ch := make(chan os.Signal)

	// signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	// go func() {
	// 	<-ch
	// 	fmt.Println("Received interrupt!")
	// 	cleanup(rw, ha)
	// 	os.Exit(1)
	// }()
}

func readData(rw *bufio.ReadWriter) {
	for {
		// read string until eol
		str, err := rw.ReadString('\n')

		if err != nil {
			log.Fatal(err)
			// log.Println("Hi", err)
		}

		if str == "" {
			// log.Println("hi1")
			return
		}

		// if string does not just contain a newline char
		// that is - if string contains some input
		if str != "\n" {

			// If peer exits/dies continue executing another routine, don't exit!
			if str == "Peer Exit\n" {
				return
			}

			chain := make([]Block, 0)
			if err := json.Unmarshal([]byte(str), &chain); err != nil {
				log.Fatal(err)
			}

			mutex.Lock()

			// after getting the blockchain from the client, print it on your own terminal
			if len(chain) > len(Blockchain) {
				Blockchain = chain
				bytes, err := json.MarshalIndent(Blockchain, "", "	")
				if err != nil {
					log.Fatal(err)
				}

				// your competitor has finished mining the blockchain
				mined = true
				fmt.Printf("\x1b[32m%s\x1b[0m>", string(bytes))
			}
			mutex.Unlock()
		}
	}
}

func writeData(rw *bufio.ReadWriter) {
	go func() {
		for {
			// fmt.Println("Sleeping...")
			time.Sleep(1 * time.Second)
			mutex.Lock()

			// fmt.Println("Sleeping11...")

			bytes, err := json.Marshal(Blockchain)

			if err != nil {
				log.Println(err)
			}
			mutex.Unlock()

			mutex.Lock()
			rw.WriteString(fmt.Sprintf("%s\n", string(bytes)))
			rw.Flush()
			mutex.Unlock()
		}
	}()

	stdReader := bufio.NewReader(os.Stdin)

	for {
		// fmt.Println("Hi..")
		fmt.Print(">")
		sendData, err := stdReader.ReadString('\n')

		if err != nil {
			log.Fatal(err)
		}

		sendData = strings.Replace(sendData, "\n", "", -1)
		bpm, err := strconv.Atoi(sendData)

		if err != nil {
			log.Fatal(err)
		}

	gen:
		newBlock, _ := generateBlock(Blockchain[len(Blockchain)-1], bpm)

		if mined == true {
			mined = false
			goto gen
		}

		if isBlockValid(newBlock, Blockchain[len(Blockchain)-1]) {
			mutex.Lock()
			Blockchain = append(Blockchain, newBlock)
			mutex.Unlock()
		}

		bytes, err := json.Marshal(Blockchain)
		if err != nil {
			log.Println(err)
		}

		spew.Dump(Blockchain)

		mutex.Lock()
		rw.WriteString(fmt.Sprintf("%s\n", string(bytes)))
		rw.Flush()
		mutex.Unlock()

	}
}

func isBlockValid(nextBlock, prevBlock Block) bool {
	if prevBlock.Index+1 != nextBlock.Index {
		return false
	}

	if prevBlock.Hash != nextBlock.PrevHash {
		return false
	}

	if calculateHash(nextBlock) != nextBlock.Hash {
		return false
	}

	return true
}

// Calculate the hash of the given block
func calculateHash(block Block) string {
	record := strconv.Itoa(block.Index) + block.Timestamp + strconv.Itoa(block.BPM) + block.PrevHash + block.Nonce
	// create a new sha256 hash instance
	hash := sha256.New()
	// write the record to the hash instance
	hash.Write([]byte(record))
	// return the resulting hashed slice
	hashed := hash.Sum(nil)
	// encode the hashed slice to a hex string
	return hex.EncodeToString(hashed)
}

// generate a new block that is "to be" appended to the blockchain
func generateBlock(prevBlock Block, BPM int) (Block, error) {
	var nextBlock Block

	nextBlock.Index = prevBlock.Index + 1

	nextBlock.Timestamp = (time.Now()).String()

	nextBlock.BPM = BPM
	nextBlock.PrevHash = prevBlock.Hash

	nextBlock.Difficulty = difficulty

	// Loop until the right nonce value is found that generates a valid hash
	for i := 0; ; i++ {
		hex := fmt.Sprintf("%x", i)
		nextBlock.Nonce = hex

		nextHash := calculateHash(nextBlock)
		if !isHashValid(nextHash, nextBlock.Difficulty) {
			fmt.Println(nextHash, " do more work!")
			time.Sleep(time.Second)
			continue
		} else {
			fmt.Println(nextHash, " do more work!")
			nextBlock.Hash = nextHash
			break
		}
	}
	// nextBlock.Hash = calculateHash(nextBlock)

	return nextBlock, nil
}

// Each node in the network checks whether the block
// its receiving actually contains a valid hash
func isHashValid(hash string, difficulty int) bool {
	prefix := strings.Repeat("0", difficulty)
	return strings.HasPrefix(hash, prefix)
}

func cleanup(rw *bufio.ReadWriter, ha host.Host, peerID peer.ID) {
	fmt.Println("Node dying.. exiting gracefully..")
	// ha.RemoveStreamHandler("/p2p/1.0.0")
	// ha.Network().ClosePeer(peerID)
	mutex.Lock()
	// Inform to the connected peer that you are exiting
	rw.WriteString("Peer Exit\n")
	rw.Flush()
	mutex.Unlock()
}
