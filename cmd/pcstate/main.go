// Command pcstate runs the State platform.
//
//	pcstate dev [flags]
//
// The dev command starts the State together with a local Commonwealth that
// settles every eligibility count automatically, and serves the State's web
// interface. Everything is created fresh on each run and removed on exit.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/polycentric-commonwealth/platform-state/internal/devnet"
	"github.com/polycentric-commonwealth/platform-state/internal/web"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "dev" {
		fmt.Fprintln(os.Stderr, "usage: pcstate dev [-addr 127.0.0.1:8080] [-password PASSWORD] [-residents 60] [-per-person 25.50] [-name NAME]")
		os.Exit(2)
	}
	if err := dev(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "pcstate:", err)
		os.Exit(1)
	}
}

func dev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address for the web interface")
	password := fs.String("password", "", "officials' password (default: generated)")
	residents := fs.Int("residents", 60, "number of sample residents")
	perPerson := fs.String("per-person", "25.50", "dividend per person the local Commonwealth pays")
	name := fs.String("name", "Example State", "the State's name")
	fs.Parse(args)

	if *password == "" {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		*password = strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	}

	dir, err := os.MkdirTemp("", "pcstate-dev-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	net, err := devnet.Start(devnet.Options{
		DataDir: dir, StateName: *name, PerPerson: *perPerson, Residents: *residents,
		SettlementDelay: 1500 * time.Millisecond,
	})
	if err != nil {
		return err
	}
	defer net.Close()

	site, err := web.New(net.State, net.Store, *password, false)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *addr, Handler: site, ReadHeaderTimeout: 10 * time.Second}

	fmt.Printf(`
%s is running on a local development network.

  Web interface        http://%s/
  Officials' password  %s

  State instance       %s (protocol at %spc/)
  Commonwealth         %s, settling %s XCU per person

Data is kept in %s and removed when you stop the server (Ctrl+C).

`, *name, *addr, *password, devnet.StateID, net.StateBaseURL, devnet.CommonwealthID, *perPerson, dir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Println("shutting down")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
