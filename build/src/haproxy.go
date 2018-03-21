package main

import (
	"bytes"
	"io"
	"bufio"
	"log"
	"net"
	"strings"
	"strconv"
	"os"
  "os/exec"
	"syscall"
	"container/list"
)

var pid int

func haproxy() {
	cmd := exec.Command("haproxy", "-db", "-f", "/usr/local/etc/haproxy/haproxy.cfg")
	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start haproxy: %s", err.Error())
	}
	pid = cmd.Process.Pid

	var stdoutBuf, stderrBuf bytes.Buffer
	stdout := io.MultiWriter(os.Stdout, &stdoutBuf)
	stderr := io.MultiWriter(os.Stderr, &stderrBuf)

	go func() {
		_, _ = io.Copy(stdout, stdoutPipe)
	}()

	go func() {
		_, _ = io.Copy(stderr, stderrPipe)
	}()
}

func defaultBackend(done chan int, port int, handle func(net.Conn)) {
	defer doneChan(done)
	listener, err := net.Listen("tcp", "0.0.0.0:" + httpSwarmRouterPort)
	if err != nil {
		log.Printf("Listening error: %s", err.Error())
		return
	}
	log.Printf("Listening started on port: %d", port)
	for {
		connection, err := listener.Accept()
		if err != nil {
			log.Printf("Accept error: %s", err.Error())
			return
		}
		go handle(connection)
	}
}

func doneChan(done chan int){
	done <- 1
}

func addBackend(hostname string) {
	// Add new backend to backend memory map (ttl map pending)
	log.Printf("Adding %s to swarm-router", hostname)
	httpBackends[hostname], _ = strconv.Atoi(httpBackendsDefaultPort)
	/*scanner := bufio.NewWordScanner(os.Getenv("BACKEND_PORTS")))
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), hostname) {

			break
		}
	}*/
	// Generate new haproxy configuration
	executeTemplate("/usr/local/etc/haproxy/haproxy.tmpl", "/usr/local/etc/haproxy/haproxy.cfg")
	// Restart haproxy using USR2 signal
	log.Printf("Pid: %d", pid)
	proc, err := os.FindProcess(pid)
	if err != nil {
	    log.Printf(err.Error())
	}
	proc.Signal(syscall.SIGUSR2)
}

func copy(dst io.WriteCloser, src io.Reader) {
	io.Copy(dst, src)
	dst.Close()
}

func httpHandler(downstream net.Conn) {
	reader := bufio.NewReader(downstream)
	hostname := ""
	readLines := list.New()
	for hostname == "" {
		bytes, _, error := reader.ReadLine()
		if error != nil {
			log.Printf("Error reading", error)
			downstream.Close()
			return
		}
		line := string(bytes)
		readLines.PushBack(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Host: ") {
			hostname = strings.TrimPrefix(line, "Host: ")
			if strings.ContainsAny(hostname, ":") {
				hostname, _, _ = net.SplitHostPort(hostname)
			}
			break
		}
	}
	// Check if backend was already added
	if httpBackends[hostname] == 0 {
		// Resolve target ip address for hostname
		backendIPAddr, err := net.ResolveIPAddr("ip", hostname)
		if err != nil {
				log.Printf("Error resolving ip address for: %s", err.Error())
				return
		}
		// Get swarm-router ip adresses
		ownIPAddrs, err := net.InterfaceAddrs()
		if err != nil {
				log.Printf("Error resolving own ip address: %s", err.Error())
				return
		}
		for _, ownIPAddr := range ownIPAddrs {
			if ownIPNet, ok := ownIPAddr.(*net.IPNet); ok && !ownIPNet.IP.IsLoopback() {
				if ownIPNet.IP.To4() != nil {
					// Check if target ip is member of attached swarm networks
					if ownIPNet.Contains(backendIPAddr.IP) {
						addBackend(hostname)
						upstream, err := net.Dial("tcp", hostname + ":" + httpBackendsDefaultPort)
						if err != nil {
							log.Printf("Backend connection error: %s", err.Error())
							downstream.Close()
							return
						}
						for element := readLines.Front(); element != nil; element = element.Next() {
							line := element.Value.(string)
							upstream.Write([]byte(line))
							upstream.Write([]byte("\n"))
						}
						go copy(upstream, reader)
						go copy(downstream, upstream)
						break
					} else {
						log.Printf("Target ip address %s for %s is not part of swarm network %s", backendIPAddr.String(), hostname, ownIPNet)
						downstream.Close()
					}
				}
			}
		}
	}
}