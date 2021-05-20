package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/caiguanhao/mei/base"
	"go.bug.st/serial"
)

var verbosive bool

func main() {
	address := flag.String("address", "127.0.0.1:22222", "address to listen to (or target address when using --call)")
	device := flag.String("device", "/dev/ttyS0", "device")
	baudRate := flag.Int("baud-rate", 9600, "baud rate")
	search := flag.Bool("search", false, "search available serial ports and find the possible one and exit")
	call := flag.String("call", "", "issue a command (ns,secs,dimes|eba|eca|dba|dca|count|gc,dimes) and exit")
	flag.BoolVar(&verbosive, "verbose", false, "show more info")
	flag.Parse()

	if *call != "" {
		processCliCommand(*address, *call)
		return
	}

	if *search {
		if verbosive {
			log.SetOutput(os.Stderr)
		} else {
			log.SetOutput(&bytes.Buffer{})
		}
		port := searchPort(*baudRate)
		if port == "" {
			fmt.Println("no suitable port found")
		} else {
			fmt.Println(port)
		}
		return
	}

	b := base.NewBase(verbosive)
	err := b.Open(*device, func() (base.Port, error) {
		return serial.Open(*device, &serial.Mode{
			BaudRate: *baudRate,
			DataBits: 8,
			Parity:   serial.NoParity,
			StopBits: serial.OneStopBit,
		})
	})
	if err != nil {
		log.Fatalln(err)
	}

	err = rpc.RegisterName("MEI", b.ValueForRPCRegister())
	if err != nil {
		log.Fatalln(err)
	}

	tcpAddr, err := net.ResolveTCPAddr("tcp", *address)
	if err != nil {
		log.Fatalln(err)
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		log.Fatalln(err)
	}

	log.Println("listening", tcpAddr.String())
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go rpc.ServeCodec(jsonrpc.NewServerCodec(conn))
	}
}

func processCliCommand(address, input string) {
	client, err := jsonrpc.Dial("tcp", address)
	if err != nil {
		log.Fatalln(err)
	}
	parts := strings.Split(input, ",")
	var command string
	var params interface{}
	if parts[0] == "ns" {
		command = "MEI.NewSession"
		seconds, dimes := 0, 0
		if len(parts) > 1 {
			seconds, _ = strconv.Atoi(parts[1])
			if len(parts) > 2 {
				dimes, _ = strconv.Atoi(parts[2])
			}
		}
		params = &base.NewSessionArgs{
			Seconds: seconds,
			Dimes:   dimes,
		}
	} else if parts[0] == "eba" {
		command = "MEI.EnableBillAcceptor"
	} else if parts[0] == "eca" {
		command = "MEI.EnableCoinAcceptor"
	} else if parts[0] == "dba" {
		command = "MEI.DisableBillAcceptor"
	} else if parts[0] == "dca" {
		command = "MEI.DisableCoinAcceptor"
	} else if parts[0] == "max" {
		command = "MEI.GetMaxDimesForGiveChange"
	} else if parts[0] == "gc" {
		command = "MEI.GiveChange"
		var dimes int
		if len(parts) > 1 {
			dimes, _ = strconv.Atoi(parts[1])
		}
		params = &base.GiveChangeArgs{
			Dimes: dimes,
		}
	} else {
		log.Fatalln("invalid command")
	}
	var reply interface{}
	err = client.Call(command, params, &reply)
	if err != nil {
		log.Fatalln(err)
	}
	b, _ := json.Marshal(reply)
	fmt.Println(string(b))
}

func searchPort(baudRate int) string {
	portPaths, err := serial.GetPortsList()
	if err != nil {
		return ""
	}
	pathChan := make(chan string)
	for _, path := range portPaths {
		go func(path string) {
			b := base.NewBase(verbosive)
			defer b.Close()
			if b.Open(path, func() (base.Port, error) {
				return serial.Open(path, &serial.Mode{
					BaudRate: baudRate,
					DataBits: 8,
					Parity:   serial.NoParity,
					StopBits: serial.OneStopBit,
				})
			}) == nil && b.Check() == nil {
				pathChan <- path
			}
		}(path)
	}
	select {
	case path := <-pathChan:
		return path
	case <-time.After(11 * time.Second):
		return ""
	}
}
