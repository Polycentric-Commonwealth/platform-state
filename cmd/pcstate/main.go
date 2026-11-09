// Command pcstate runs the State platform.
//
//	pcstate dev [flags]                      try the platform on a local development network
//	pcstate init -dir DIR [flags]            create a deployment
//	pcstate attestation add -dir DIR FILE    add the parent's attestation of the State's key
//	pcstate official add|reset|disable|list  manage officials' accounts
//	pcstate serve -dir DIR                   run a deployment
//	pcstate devcommonwealth run|attest       a stand-in Commonwealth for testing deployments
//
// Run any command with -h for its flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/polycentric-commonwealth/platform-state/internal/deploy"
	"github.com/polycentric-commonwealth/platform-state/internal/devnet"
	"github.com/polycentric-commonwealth/platform-state/internal/store"
	"github.com/polycentric-commonwealth/platform-state/internal/web"
)

const usage = `usage:
  pcstate dev [-addr 127.0.0.1:8080] [-residents 60] [-per-person 25.50] [-name NAME] [-ghosts 0]
  pcstate init -dir DIR -instance ID -name NAME -base-url URL -parent ID -anchor ID=KEY [flags]
  pcstate attestation add -dir DIR FILE
  pcstate official add -dir DIR -user USER -name NAME -role admin|registrar|treasurer
  pcstate official reset|disable -dir DIR -user USER
  pcstate official list -dir DIR
  pcstate serve -dir DIR
  pcstate devcommonwealth run -dir DIR -instance ID -base-url URL -listen ADDR [flags]
  pcstate devcommonwealth attest -dir DIR -instance ID DESCRIPTION.json
`

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "dev":
		err = dev(rest)
	case "init":
		err = initCmd(rest)
	case "attestation":
		err = attestationCmd(rest)
	case "official":
		err = officialCmd(rest)
	case "serve":
		err = serve(rest)
	case "devcommonwealth":
		err = devCommonwealth(rest)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pcstate:", err)
		os.Exit(1)
	}
}

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func pairs(list []string, what string) (map[string]string, error) {
	m := map[string]string{}
	for _, item := range list {
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("%s must be written ID=VALUE, got %q", what, item)
		}
		m[k] = v
	}
	return m, nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func dev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address for the web interface")
	password := fs.String("password", "", "the admin official's password (default: generated)")
	residents := fs.Int("residents", 60, "number of sample residents")
	perPerson := fs.String("per-person", "25.50", "dividend per person the local Commonwealth pays")
	name := fs.String("name", "Example State", "the State's name")
	ghosts := fs.Int("ghosts", 0, "invent this many residents, to watch the audit catch an inflated count")
	fs.Parse(args)

	if *password == "" {
		var err error
		if *password, err = store.GeneratePassword(); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp("", "pcstate-dev-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	net, err := devnet.Start(devnet.Options{
		DataDir: dir, StateName: *name, PerPerson: *perPerson, Residents: *residents,
		SettlementDelay: 1500 * time.Millisecond, Ghosts: *ghosts,
	})
	if err != nil {
		return err
	}
	defer net.Close()
	if err := net.Store.AddOfficial("admin", "Development admin", store.RoleAdmin, *password); err != nil {
		return err
	}

	site, err := web.New(net.State, net.Store, false)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *addr, Handler: site, ReadHeaderTimeout: 10 * time.Second}

	fmt.Printf(`
%s is running on a local development network.

  Web interface   http://%s/
  Sign in as      admin
  Password        %s

  State instance  %s (protocol at %spc/)
  Commonwealth    %s, settling %s XCU per person

Data is kept in %s and removed when you stop the server (Ctrl+C).

`, *name, *addr, *password, devnet.StateID, net.StateBaseURL, devnet.CommonwealthID, *perPerson, dir)

	ctx, stop := signalContext()
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

func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", "", "directory for the new deployment")
	instance := fs.String("instance", "", "the State's instance identifier, a domain name such as state.example")
	name := fs.String("name", "", "the State's name")
	baseURL := fs.String("base-url", "", "public https URL of the platform, such as https://state.example/")
	listen := fs.String("listen", ":8443", "address to serve on")
	tlsCert := fs.String("tls-cert", "", "certificate file, to serve HTTPS directly (otherwise run behind a TLS proxy)")
	tlsKey := fs.String("tls-key", "", "private key file for -tls-cert")
	parent := fs.String("parent", "", "the instance that attests this State: its Region, or the Commonwealth")
	commonwealth := fs.String("commonwealth", "", "the Commonwealth instance (default: the first anchor)")
	caFile := fs.String("ca-file", "", "extra PEM certificates to trust when reaching other instances")
	threshold := fs.Int("witness-threshold", 1, "how many witnesses must cosign a checkpoint")
	admin := fs.String("admin", "admin", "username of the first admin official")
	adminName := fs.String("admin-name", "Administrator", "name of the first admin official")
	var anchors, witnesses, peers listFlag
	fs.Var(&anchors, "anchor", "trust anchor, ID=ed25519:KEY (repeatable)")
	fs.Var(&witnesses, "witness", "witness instance (repeatable; default: the Commonwealth)")
	fs.Var(&peers, "peer", "reach an instance at another origin, ID=https://host:port (repeatable)")
	fs.Parse(args)
	if *dir == "" {
		return errors.New("init: -dir is required")
	}
	anchorMap, err := pairs(anchors, "-anchor")
	if err != nil {
		return err
	}
	peerMap, err := pairs(peers, "-peer")
	if err != nil {
		return err
	}
	cfg := deploy.Config{
		Instance: *instance, Name: *name, BaseURL: *baseURL, Listen: *listen, TLSCert: *tlsCert, TLSKey: *tlsKey,
		Parent: *parent, Commonwealth: *commonwealth, Witnesses: witnesses, WitnessThreshold: *threshold,
		Peers: peerMap, CAFile: *caFile,
	}
	var ids []string
	for id := range anchorMap {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cfg.Anchors = append(cfg.Anchors, deploy.Anchor{Instance: id, PublicKey: anchorMap[id]})
	}
	if cfg.Commonwealth == "" && len(ids) > 0 {
		cfg.Commonwealth = ids[0]
	}
	if len(cfg.Peers) == 0 {
		cfg.Peers = nil
	}
	if _, err := deploy.Init(*dir, cfg); err != nil {
		return err
	}
	db, err := deploy.OpenStore(*dir)
	if err != nil {
		return err
	}
	defer db.Close()
	password, err := store.GeneratePassword()
	if err != nil {
		return err
	}
	if err := db.AddOfficial(*admin, *adminName, store.RoleAdmin, password); err != nil {
		return err
	}
	fmt.Printf(`Created the deployment for %s in %s.

First admin official: %s
Password (shown once): %s

Next:
  1. Send %s/%s to %s and ask for an attestation of its key
     (a parent can use: pcrecord attest -priv PARENT.key -issuer %s -key KEYID -layer LAYER %s/%s).
  2. pcstate attestation add -dir %s ATTESTATION.json
  3. pcstate serve -dir %s
`, *instance, *dir, *admin, password, *dir, deploy.DescriptionFile, *parent, *parent, *dir, deploy.DescriptionFile, *dir, *dir)
	return nil
}

func attestationCmd(args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: pcstate attestation add -dir DIR FILE")
	}
	fs := flag.NewFlagSet("attestation add", flag.ExitOnError)
	dir := fs.String("dir", "", "deployment directory")
	fs.Parse(args[1:])
	if *dir == "" || fs.NArg() != 1 {
		return errors.New("usage: pcstate attestation add -dir DIR FILE")
	}
	data, err := readFile(fs.Arg(0))
	if err != nil {
		return err
	}
	a, err := deploy.AddAttestation(*dir, data)
	if err != nil {
		return err
	}
	fmt.Printf("Added the attestation of %d key(s). It is published and checked against %s when the platform next starts.\n", len(a.Keys), a.Subject)
	return nil
}

func officialCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pcstate official add|reset|disable|list -dir DIR ...")
	}
	sub := args[0]
	fs := flag.NewFlagSet("official "+sub, flag.ExitOnError)
	dir := fs.String("dir", "", "deployment directory")
	user := fs.String("user", "", "username")
	name := fs.String("name", "", "full name (add)")
	role := fs.String("role", store.RoleRegistrar, "admin, registrar or treasurer (add)")
	fs.Parse(args[1:])
	if *dir == "" {
		return errors.New("official: -dir is required")
	}
	db, err := deploy.OpenStore(*dir)
	if err != nil {
		return err
	}
	defer db.Close()
	switch sub {
	case "list":
		list, err := db.Officials()
		if err != nil {
			return err
		}
		for _, o := range list {
			status := "active"
			if o.Disabled {
				status = "disabled"
			}
			fmt.Printf("%-20s %-10s %-9s %s\n", o.Username, o.Role, status, o.Name)
		}
		return nil
	case "add", "reset":
		if *user == "" {
			return errors.New("official: -user is required")
		}
		password, err := store.GeneratePassword()
		if err != nil {
			return err
		}
		if sub == "add" {
			err = db.AddOfficial(*user, *name, *role, password)
		} else if err = db.SetPassword(*user, password); err == nil {
			err = db.SetDisabled(*user, false)
		}
		if err != nil {
			return err
		}
		if err := db.LogAction("(command line)", map[string]string{"add": "added official", "reset": "reset password"}[sub], *user); err != nil {
			return err
		}
		fmt.Printf("Password for %s (shown once): %s\n", *user, password)
		return nil
	case "disable":
		if *user == "" {
			return errors.New("official: -user is required")
		}
		if err := db.SetDisabled(*user, true); err != nil {
			return err
		}
		fmt.Printf("Disabled %s and signed them out everywhere.\n", *user)
		return db.LogAction("(command line)", "disabled official", *user)
	}
	return fmt.Errorf("official: unknown command %q", sub)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", "", "deployment directory")
	fs.Parse(args)
	if *dir == "" {
		return errors.New("serve: -dir is required")
	}
	in, err := deploy.Open(*dir)
	if err != nil {
		return err
	}
	defer in.Close()
	log.SetPrefix("pcstate: ")
	in.Logf = log.Printf
	log.Printf("%s (%s) serving on %s, public at %s", in.Config.Name, in.Config.Instance, in.Config.Listen, in.Config.BaseURL)
	ctx, stop := signalContext()
	defer stop()
	err = in.Serve(ctx)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	log.Println("stopped")
	return err
}

func devCommonwealth(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pcstate devcommonwealth run|attest ...")
	}
	sub := args[0]
	fs := flag.NewFlagSet("devcommonwealth "+sub, flag.ExitOnError)
	dir := fs.String("dir", "", "directory for the stand-in Commonwealth's key")
	instance := fs.String("instance", "commonwealth.example", "its instance identifier")
	baseURL := fs.String("base-url", "", "its public https URL, ending in /")
	listen := fs.String("listen", ":9443", "address to serve on")
	tlsCert := fs.String("tls-cert", "", "certificate file")
	tlsKey := fs.String("tls-key", "", "private key file")
	caFile := fs.String("ca-file", "", "extra PEM certificates to trust when reaching States")
	perPerson := fs.String("per-person", "25.50", "dividend per person, in XCU")
	var peers listFlag
	fs.Var(&peers, "peer", "reach an instance at another origin, ID=https://host:port (repeatable)")
	fs.Parse(args[1:])
	if *dir == "" {
		return errors.New("devcommonwealth: -dir is required")
	}
	switch sub {
	case "attest":
		if fs.NArg() != 1 {
			return errors.New("usage: pcstate devcommonwealth attest -dir DIR DESCRIPTION.json")
		}
		priv, err := devnet.LoadOrCreateKey(*dir)
		if err != nil {
			return err
		}
		data, err := readFile(fs.Arg(0))
		if err != nil {
			return err
		}
		env, err := devnet.Attest(*instance, priv, data)
		if err != nil {
			return err
		}
		out, err := env.Marshal()
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(out)
		return err
	case "run":
		peerMap, err := pairs(peers, "-peer")
		if err != nil {
			return err
		}
		priv, err := devnet.LoadOrCreateKey(*dir)
		if err != nil {
			return err
		}
		log.SetPrefix("devcommonwealth: ")
		log.Printf("serving %s on %s; States pin it with -anchor %s", *instance, *listen, devnet.Anchor(*instance, priv))
		ctx, stop := signalContext()
		defer stop()
		err = devnet.RunCommonwealth(ctx, devnet.StandaloneOptions{
			Dir: *dir, Instance: *instance, BaseURL: *baseURL, Listen: *listen, TLSCert: *tlsCert, TLSKey: *tlsKey,
			CAFile: *caFile, Peers: peerMap, PerPerson: *perPerson, Delay: time.Second, Logf: log.Printf,
		})
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		return err
	}
	return fmt.Errorf("devcommonwealth: unknown command %q", sub)
}

func readFile(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
