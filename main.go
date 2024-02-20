// Copyright 2020 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package main is a very simple server with UDP (default), TCP, or both
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	coresdk "agones.dev/agones/pkg/sdk"
	"agones.dev/agones/pkg/util/signals"
	sdk "agones.dev/agones/sdks/go"
	"github.com/samber/lo"
	"golang.org/x/time/rate"
)

// execute the git describe command to get the version

var (
	// embed the version information at compile-time
	Version = func() string {
		out, _ := exec.Command("git", "describe", "--tags", "--dirty=.d", "--always").Output()
		if v := strings.TrimSpace(string(out)); v != "" {
			return v
		}
		return "0.0.0"
	}()

	loggerFlags = log.LstdFlags | log.Ldate | log.Ltime | log.Lmsgprefix
)

// main starts a UDP or TCP server
func main() {
	sigCtx, _ := signals.NewSigKillContext()

	shutdownDelayMin := flag.Int("automaticShutdownDelayMin", 0, "[Deprecated] If greater than zero, automatically shut down the server this many minutes after the server becomes allocated (please use automaticShutdownDelaySec instead)")
	shutdownDelaySec := flag.Int("automaticShutdownDelaySec", 0, "If greater than zero, automatically shut down the server this many seconds after the server becomes allocated (cannot be used if automaticShutdownDelayMin is set)")
	readyDelaySec := flag.Int("readyDelaySec", 0, "If greater than zero, wait this many seconds each time before marking the game server as ready")
	readyIterations := flag.Int("readyIterations", 0, "If greater than zero, return to a ready state this number of times before shutting down")
	gracefulTerminationDelaySec := flag.Int("gracefulTerminationDelaySec", 0, "Delay after we've been asked to terminate (by SIGKILL or automaticShutdownDelaySec)")

	echoBinaryPath := flag.String("echovrPath", "/echovr/bin/win10/echovr.exe", "The command to run")
	evrApiPort := flag.Int("httpPort", 6710, "The HTTP port for EchoVR to listen for API requests")
	udpBroadcastPort := flag.Int("bcastPort", 6794, "The UDP port for EchoVR to listen for UDP broadcast requests")
	loginServiceProxyPort := flag.Int("tcpPort", 6789, "The TCP port for the TCP proxy to listen for the login connection from EchoVR")
	defaultArgs := flag.String("defaultArgs", "-noovr -server -headless -noconsole -usercfgpath /data", "The default arguments to pass to echovr.exe (avoid changing this unless you know what you're doing)")
	timestepFrequency := flag.Int("timestepFrequency", 120, "The timestep frequency of the game engine")
	serverRegion := flag.String("serverRegion", "default", "The server region")
	displayName := flag.String("displayName", "EchoVR", "The display name of the echoVR server")
	verbose := flag.Bool("verbose", false, "Enable verbose logging")
	flag.Parse()

	// Configure logging
	if *verbose {
		loggerFlags |= log.Llongfile
	} else {
		loggerFlags |= log.Lshortfile
	}

	log.SetFlags(loggerFlags)

	log.Printf("Starting EchoVR wrapper v%s", Version)

	if etimestepFrequency := os.Getenv("TIMESTEP_FREQUENCY"); etimestepFrequency != "" {
		p, err := strconv.Atoi(etimestepFrequency)
		if err != nil {
			log.Fatalf("Could not parse TIMESTEP_FREQUENCY: %v", err)
		}
		timestepFrequency = &p
	}

	// Check for incompatible flags.
	if *shutdownDelayMin > 0 && *shutdownDelaySec > 0 {
		log.Fatalf("Cannot set both --automaticShutdownDelayMin and --automaticShutdownDelaySec")
	}
	if *readyIterations > 0 && *shutdownDelayMin <= 0 && *shutdownDelaySec <= 0 {
		log.Fatalf("Must set a shutdown delay if using ready iterations")
	}

	log.Print("Creating SDK instance")
	s, err := sdk.NewSDK()
	if err != nil {
		log.Fatalf("Could not connect to sdk: %v", err)
	}

	log.Print("Starting Health Ping")
	ctx, _ := context.WithCancel(context.Background())
	go doHealth(s, ctx)

	var gs *coresdk.GameServer
	gs, err = s.GameServer()
	if err != nil {
		log.Fatalf("Could not get gameserver port details: %s", err)
	}

	portsByName := make(map[string]int, len(gs.Status.Ports))
	for _, p := range gs.Status.Ports {
		portsByName[p.Name] = int(p.Port)
	}

	if v, ok := portsByName["gameHTTPAPI"]; !ok {
		evrApiPort = &v
	}
	if v, ok := portsByName["udpBroadcastPort"]; ok {
		udpBroadcastPort = &v
	}

	apiProxy := NewAPICachingProxy(evrApiPort, evrApiPort, lo.ToPtr(30))

	// Setup the login TCP Proxy
	// Get the origin address from the config

	tcpProxyAddress := fmt.Sprintf("127.0.0.1:%d", *loginServiceProxyPort)

	config := LoadConfig("/data/config.json")
	loginAddress := config.GetLoginAddress()

	loginProxy := NewTCPProxy(tcpProxyAddress, loginAddress)

	if *shutdownDelaySec > 0 {
		shutdownAfterNAllocations(s, *readyIterations, *shutdownDelaySec)
	} else if *shutdownDelayMin > 0 {
		shutdownAfterNAllocations(s, *readyIterations, *shutdownDelayMin*60)
	}

	// Build the command line
	args := []string{*echoBinaryPath}
	args = append(args, strings.Fields(*defaultArgs)...)
	args = append(args, "-serverregion", *serverRegion)
	args = append(args, "-displayname", *displayName)
	args = append(args, "-loginhost", tcpProxyAddress)
	args = append(args, "-httpport", strconv.FormatInt(int64(*evrApiPort), 10))
	args = append(args, "-port", strconv.FormatInt(int64(*udpBroadcastPort), 10))
	args = append(args, "-retries", "0")
	if *timestepFrequency > 0 {
		args = append(args, "-fixed-timestep", "-timestep", strconv.FormatInt(int64(*timestepFrequency), 10))
	} else {
		log.Printf("warning: timestep frequency is not set, tickrate will be unlimited.")
	}

	engine := NewGameEngine(*echoBinaryPath, args)

	log.Printf("Starting game engine with command: %s", strings.Join(engine.Command.Args, " "))

	loginProxy.Start(ctx, engine, apiProxy)

	engine.Start()
	go apiProxy.ListenAndProxy(ctx)

	// TODO FIXME Wait until the healtcheck says the server is connect to Nakama

	if *readyDelaySec > 0 {
		log.Printf("Waiting %d seconds before moving to ready", *readyDelaySec)
		time.Sleep(time.Duration(*readyDelaySec) * time.Second)
	}

	log.Print("Marking this server as ready")
	ready(s)

	<-sigCtx.Done()

	log.Printf("Waiting %d seconds before exiting", *gracefulTerminationDelaySec)
	time.Sleep(time.Duration(*gracefulTerminationDelaySec) * time.Second)
	os.Exit(0)
}

// UserConfig is a struct that represents the config.json.
type UserConfig struct {
	PublisherLock *string `json:"publisher_lock,omitempty"`
	APIURL        *string `json:"apiservice_host,omitempty"`
	Config        *string `json:"configservice_host,omitempty"`
	Login         *string `json:"loginservice_host,omitempty"`
	Matching      *string `json:"matchingservice_host,omitempty"`
	AIP           *string `json:"transactionservice_host,omitempty"`
	ServerDB      *string `json:"serverdb_host,omitempty"`
}

func NewUserConfig(pubLock, api, config, login, match, aip, db *string) *UserConfig {
	return &UserConfig{
		PublisherLock: pubLock,
		APIURL:        api,
		Config:        config,
		Login:         login,
		Matching:      match,
		AIP:           aip,
		ServerDB:      db,
	}
}

func LoadConfig(path string) *UserConfig {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Could not read user config: %v", err)
	}
	return UnmarshalUserConfig(b)
}
func (u *UserConfig) Save(path string) {
	if err := os.WriteFile(path, u.Marshal(), 0644); err != nil {
		log.Fatalf("Could not write user config: %v", err)
	}
}

func UnmarshalUserConfig(b []byte) *UserConfig {
	var u UserConfig
	if err := json.Unmarshal(b, &u); err != nil {
		log.Fatalf("Could not unmarshal user config: %v", err)
	}
	return &u
}
func (u *UserConfig) Marshal() []byte {
	b, err := json.Marshal(u)
	if err != nil {
		log.Fatalf("Could not marshal user config: %v", err)
	}
	return b
}

// GetLoginAddress returns the login service address (IP:Port)
func (u *UserConfig) GetLoginAddress() string {
	// Parse the loginservice URL
	p, err := url.Parse(*u.Login)
	if err != nil {
		log.Fatalf("Could not parse loginservice URL: %v", err)
	}

	if p.Port() != "" {
		return p.Host
	}

	switch p.Scheme {
	case "ws":
		fallthrough
	case "http":
		return fmt.Sprintf("%s:%d", p.Host, 80)
	case "wss":
		fallthrough
	case "https":
		return fmt.Sprintf("%s:%d", p.Host, 443)
	default:
		log.Fatalf("Unknown scheme on login URL: %s", p.Scheme)
		return "" // won't get here.
	}
}

// GameEngine is a struct that represents the game engine process.
type GameEngine struct {
	sync.RWMutex
	Command    *exec.Cmd
	BinaryPath string
	Arguments  []string
}

func NewGameEngine(path string, args []string) *GameEngine {
	gamelog := log.New(os.Stdout, "", loggerFlags)

	cmd := exec.Command(path, args...)

	// Use a FIFO buffer to capture the game's log output.
	fifofn := fmt.Sprintf("%s/logfifo", os.TempDir())
	cmd.Args = append(cmd.Args, "-logpath", fifofn)

	// Capture the game's stderr output
	go func() {
		r, err := cmd.StderrPipe()
		if err != nil {
			log.Fatalf("Could not get stdout pipe: %v", err)
		}
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			gamelog.Println(scanner.Text())
		}
	}()

	// Capture the game's stdout output.
	go func() {
		r, err := cmd.StdoutPipe()
		if err != nil {
			log.Fatalf("Could not get stdout pipe: %v", err)
		}
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			gamelog.Println(scanner.Text())
		}
	}()

	// Capture the game's stderr output.
	go func() {
		r, err := os.OpenFile(fifofn, os.O_RDONLY, 0)
		if err != nil {
			log.Fatalf("failed to open fifo file: %w", err)
		}
		if err := syscall.Mkfifo(fifofn, 0666); err != nil {
			log.Fatalf("failed to create fifo file: %w", err)
		}
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			gamelog.Println(scanner.Text())
		}
	}()

	return &GameEngine{
		Command: cmd,
	}
}

func (e *GameEngine) Start() {
	e.Lock()
	log.Printf("Starting game engine with command: %s", strings.Join(e.Command.Args, " "))
	if err := e.Command.Start(); err != nil {
		log.Fatalf("Could not start game engine: %v", err)
	}
	e.Unlock()
	if err := e.Command.Wait(); err != nil {
		log.Fatalf("Game engine exited with error: %v", err)
	}
}

// Stop kills the game engine process, and the wrapper dies with it.
func (e *GameEngine) Stop() {
	e.Lock()
	defer e.Unlock()
	if e.Command != nil && e.Command.Process != nil {
		if err := e.Command.Process.Kill(); err != nil {
			log.Fatalf("Could not kill game engine: %v", err)
		}
	}
	log.Fatal("Game engine process has been killed")
}

// The TCPProxy is used to monitor the login connection. Once established,
// if the gameserver disconnects, the TCPProxy will close the connection and
// gracefully shutdown the GameServer process.
type TCPProxy struct {
	sync.RWMutex
	localAddress  string
	remoteAddress string
	listener      net.Listener
	log           *log.Logger
}

func NewTCPProxy(localAddress, remoteAddress string) *TCPProxy {
	return &TCPProxy{
		localAddress:  localAddress,
		remoteAddress: remoteAddress,
		log:           log.New(os.Stderr, "[loginproxy]", loggerFlags),
	}
}

func (p *TCPProxy) Start(ctx context.Context, gameEngine *GameEngine, apiCachingProxy *APICachingProxy) {
	var err error
	p.listener, err = net.Listen("tcp4", p.localAddress)
	if err != nil {
		p.log.Fatalf("Failed to listen on %s: %v", p.localAddress, err)
	}

	connCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		defer gameEngine.Stop()
		for {
			conn, err := p.listener.Accept()
			if err != nil {
				log.Printf("Failed to accept connection: %v", err)
				continue
			}

			go p.handleConnection(connCtx, conn, gameEngine, apiCachingProxy)
		}
	}()
}

func (p *TCPProxy) handleConnection(ctx context.Context, localConn net.Conn, gameEngine *GameEngine, apiCachingProxy *APICachingProxy) {
	remoteConn, err := net.Dial("tcp4", p.remoteAddress)
	if err != nil {
		log.Fatalf("Failed to connect to %s: %v", p.remoteAddress, err)
	}
	// Shutdown the game engine if the connection dies

	go p.forward(localConn, remoteConn, gameEngine)
	go p.forward(remoteConn, localConn, gameEngine)

	<-ctx.Done()
	os.Exit(1)
}

func (p *TCPProxy) forward(src, dst net.Conn, game *GameEngine) {
	defer src.Close()
	defer dst.Close()
	// If the connection dies, gracefully shutdown the GameServer process

	io.Copy(src, dst)
}

type APICachingProxy struct {
	sync.RWMutex
	cachedData string
	originPort int
	listenPort int
	frequency  int
	logger     *log.Logger
	limiter    *rate.Limiter
	client     *http.Client
}

func NewAPICachingProxy(originPort, proxyPort, frequency *int) *APICachingProxy {

	return &APICachingProxy{
		cachedData: "",
		originPort: *originPort,
		listenPort: *proxyPort,
		frequency:  *frequency,
		logger:     log.New(os.Stderr, "[login ] ", loggerFlags),
		limiter:    rate.NewLimiter(rate.Limit(30), 1),
		client: &http.Client{
			Transport: &http.Transport{
				MaxConnsPerHost: 1,
				MaxIdleConns:    1,
				IdleConnTimeout: 30 * time.Second,
			},
		},
	}
}

func (p *APICachingProxy) ListenAndProxy(ctx context.Context) {
	http.HandleFunc("/session", p.handleRequest)
	http.ListenAndServe(fmt.Sprintf(":%d", p.listenPort), nil)
}

// queryAPI queries the API and caches the response.
func (p *APICachingProxy) queryAPI(ctx context.Context) string {
	url := fmt.Sprintf("http://127.0.0.1:%d/session", p.originPort)

	// Create a rate limiter that will allow us to query the API up to 30 times a second.
	// If no client is connected, the rate limiter will block the query.

	// Query the API up to 30 times a second, as long as a client is connected.
	// If no client is connected, the rate limiter will block the query.
	resp, err := http.Get(url)
	if err != nil {
		p.logger.Println("Evr GET Error:", err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		p.logger.Println("Evr Read Error:", err)
	}
	resp.Body.Close()
	return string(body)
}

func (p *APICachingProxy) handleRequest(w http.ResponseWriter, r *http.Request) {
	var data string
	if p.limiter.Allow() {
		// Allow the request even if the rate limit is exceeded
		ctx, _ := context.WithTimeout(r.Context(), 1000*time.Millisecond/30-3)
		p.Lock()
		data = p.queryAPI(ctx)
		p.cachedData = data
		p.Unlock()

	} else {
		p.RLock()
		data = p.cachedData
		p.RUnlock()
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, data)
}

// shutdownAfterNAllocations creates a callback to automatically shut down
// the server a specified number of seconds after the server becomes
// allocated the Nth time.
//
// The algorithm is:
//
//  1. Move the game server back to ready N times after it is allocated
//  2. Shutdown the game server after the Nth time is becomes allocated
//
// This follows the integration pattern documented on the website at
// https://agones.dev/site/docs/integration-patterns/reusing-gameservers/
func shutdownAfterNAllocations(s *sdk.SDK, readyIterations, shutdownDelaySec int) {
	gs, err := s.GameServer()
	if err != nil {
		log.Fatalf("Could not get game server: %v", err)
	}
	log.Printf("Initial game Server state = %s", gs.Status.State)

	m := sync.Mutex{} // protects the following two variables
	lastAllocated := gs.ObjectMeta.Annotations["agones.dev/last-allocated"]
	remainingIterations := readyIterations

	if err := s.WatchGameServer(func(gs *coresdk.GameServer) {
		m.Lock()
		defer m.Unlock()
		la := gs.ObjectMeta.Annotations["agones.dev/last-allocated"]
		log.Printf("Watch Game Server callback fired. State = %s, Last Allocated = %q", gs.Status.State, la)
		if lastAllocated != la {
			log.Println("Game Server Allocated")
			lastAllocated = la
			remainingIterations--
			// Run asynchronously
			go func(iterations int) {
				time.Sleep(time.Duration(shutdownDelaySec) * time.Second)

				if iterations > 0 {
					log.Println("Moving Game Server back to Ready")
					readyErr := s.Ready()
					if readyErr != nil {
						log.Fatalf("Could not set game server to ready: %v", readyErr)
					}
					log.Println("Game Server is Ready")
					return
				}

				log.Println("Moving Game Server to Shutdown")
				if shutdownErr := s.Shutdown(); shutdownErr != nil {
					log.Fatalf("Could not shutdown game server: %v", shutdownErr)
				}
				// The process will exit when Agones removes the pod and the
				// container receives the SIGTERM signal
			}(remainingIterations)
		}
	}); err != nil {
		log.Fatalf("Could not watch Game Server events, %v", err)
	}
}

// ready attempts to mark this gameserver as ready
func ready(s *sdk.SDK) {
	err := s.Ready()
	if err != nil {
		log.Fatalf("Could not send ready message")
	}
}

// doHealth sends the regular Health Pings
func doHealth(sdk *sdk.SDK, ctx context.Context) {
	tick := time.Tick(2 * time.Second)
	for {
		log.Printf("Health Ping")
		err := sdk.Health()
		if err != nil {
			log.Fatalf("Could not send health ping, %v", err)
		}
		select {
		case <-ctx.Done():
			log.Print("Stopped health pings")
			return
		case <-tick:
		}
	}
}
