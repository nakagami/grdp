package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/icodeface/grdp"
	"github.com/icodeface/grdp/glog"
)

var (
	Host     string
	User     string
	Password string
	Passfile string
)

func init() {
	flag.StringVar(&Host, "host", "", "Target rdp server ip:port")
	flag.StringVar(&User, "user", "Administrator", "Name of the client send to the server, [Domain\\]{User}")
	flag.StringVar(&Password, "password", "", "Password")
	flag.StringVar(&Passfile, "passfile", "", "Password file path")
	flag.Parse()

	if Host == "" || User == "" || (Password == "" && Passfile == "") {
		flag.Usage()
		os.Exit(0)
	}
	if Password == "" {
		if body, err := os.ReadFile(Passfile); err != nil {
			fmt.Printf("ERROR: Passfile read failed, %s\n", err)
			os.Exit(1)
		} else {
			Password = strings.Trim(string(body), "\r\n")
		}
	}
}

func main() {
	fmt.Printf("Show: Host=%s, User=%s, Password=********\n", Host, User)
	fmt.Printf("---\n")

	client := grdp.NewClient(Host)
	err := client.Login(User, Password)
	if err != nil {
		fmt.Printf("connect failed: %#v\n", err)
		os.Exit(2)
		return
	}
	defer client.Close()

	fmt.Printf("connected!\n")

	sig := make(chan struct{})
	once := new(sync.Once)
	done := func() {
		once.Do(func() {
			close(sig)
		})
	}

	client.OnError(func(e error) {
		fmt.Printf("%s Error = %#v\n", time.Now(), e)
		done()
	})
	client.OnSuccess(func() {
		fmt.Printf("%s Success\n", time.Now())
	})
	client.OnReady(func() {
		fmt.Printf("%s Ready\n", time.Now())
	})
	client.OnClose(func() {
		fmt.Printf("%s Close\n", time.Now())
		done()
	})
	client.OnUpdate(func(_ []pdu.BitmapData) {
		fmt.Printf("%s Update\n", time.Now())
	})

	fmt.Printf("waiting...\n")
	<-sig
}
