package main

import (
	"bitbucket.org/chrj/smtpd"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/miekg/dns"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Cert string
	Key  string
	Host string
	Bind string
	Port string
	Tls  string
	Time string
	Url  string
}

type Alias struct {
	Source      string
	Destination string
}

var config_file = flag.String("c", "/etc/relayd/relayd.conf", "config file")
var cert_file = flag.String("cf", "", "certificate file")
var cert_key = flag.String("ck", "", "certificate key file")
var force_tls = flag.Bool("tls", true, "force tls")
var bind_port = flag.Int("p", 25, "server port")
var bind_interface = flag.String("i", "", "server interface")
var hostname = flag.String("h", "localhost.localdomain", "server hostname")
var refresh_time = flag.Int("r", 300, "refresh time in seconds")
var alias_url = flag.String("u", "", "aliases fetch url")
var show_help = flag.Bool("help", false, "show help")
var show_version = flag.Bool("version", false, "print version")

func init() {

}

func GetOutboundIP() string {
	conn, err := net.Dial("udp", "1.2.3.4:80")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().String()
	idx := strings.LastIndex(localAddr, ":")

	return localAddr[0:idx]
}

func fetchEmailAliases(url string) ([]Alias, error) {
	var httpClient = &http.Client{Timeout: 10 * time.Second}
	var aliases []Alias

	response, err := httpClient.Get(url)

	if response != nil {
		defer response.Body.Close()
	}

	if err != nil {
		return nil, err
	}

	if response.StatusCode != 200 {
		return nil, errors.New("failed to fetch aliases")
	}

	data, err := ioutil.ReadAll(response.Body)
	body := string(data)

	lines := strings.Split(body, "\n")

	for _, line := range lines {
		ix := strings.IndexAny(line, " \t")
		if ix > 0 {
			source := strings.TrimSpace(line[:ix])
			dest := strings.TrimSpace(line[ix+1:])
			alias := Alias{source, dest}
			aliases = append(aliases, alias)
		}
	}

	log.Printf("fetched %d aliases", len(aliases))

	return aliases, err
}

func getAlias(aliases []Alias, recipient string) (Alias, error) {
	var err error
	for _, alias := range aliases {
		if alias.Source == recipient {
			return alias, err
		}
	}
	return Alias{}, errors.New("recipient not found in alias table")
}

func getMX(domain_name string) string {
	config, _ := dns.ClientConfigFromFile("/etc/resolv.conf")
	c := new(dns.Client)
	m := new(dns.Msg)
	fqdn := domain_name + "."
	m.SetQuestion(fqdn, dns.TypeMX)
	m.RecursionDesired = true
	r, _, err := c.Exchange(m, config.Servers[0]+":"+config.Port)
	if err != nil {
		log.Println(err)
		return ""
	}
	if r.Rcode != dns.RcodeSuccess {
		log.Println("name lookup failed with code ", r.Rcode)
		return ""
	}

	for _, a := range r.Answer {
		if mx, ok := a.(*dns.MX); ok {
			str := mx.String()
			ix := strings.LastIndexAny(str, " \t")
			if ix > 0 && len(str) > 3 {
				str = strings.TrimSpace(str[ix+1:])
				return str[:len(str)-1]
			}
		}
	}

	return ""
}

func main() {
	var config Config

	flag.Parse()

	if *show_help != false {
		flag.PrintDefaults()
		os.Exit(0)
	}

	if *show_version != false {
		fmt.Println("Relayd v0.1.0")
		os.Exit(0)
	}

	if *config_file != "" {
		log.Println("loading", *config_file)
	}

	json_data, err := ioutil.ReadFile(*config_file)
	json.Unmarshal(json_data, &config)

	if err != nil {
		fmt.Println(err)
		os.Exit(-1)
	}

	if config.Host == "" {
		config.Host = *hostname
	}

	if config.Bind == "" {
		if *bind_interface == "" {
			config.Bind = GetOutboundIP()
		} else {
			config.Bind = *bind_interface
		}
	}

	if config.Port == "" {
		config.Port = strconv.Itoa(*bind_port)
	}

	if *cert_file != "" {
		config.Cert = *cert_file
	}

	if *cert_key != "" {
		config.Key = *cert_key
	}

	if config.Tls != "" {
		if config.Tls == "false" {
			*force_tls = false
		} else if config.Tls == "true" {
			*force_tls = true
		}
	}

	if config.Time != "" {
		i, strerr := strconv.Atoi(config.Time)
		if strerr == nil {
			*refresh_time = i
		}
	}

	if config.Url != "" {
		if *alias_url == "" {
			*alias_url = config.Url
		}
	}

	if *alias_url == "" {
		log.Fatal("need alias fetch url")
		os.Exit(-3)
	}

	log.Println("loading certificate", config.Cert, config.Key)
	cert, err := tls.LoadX509KeyPair(config.Cert, config.Key)

	if err != nil {
		fmt.Println(err)
		os.Exit(-4)
	}

	signal_chan := make(chan os.Signal, 1)
	signal.Notify(signal_chan, syscall.SIGHUP)

	aliases, err := fetchEmailAliases(*alias_url)

	go func() {
		for {
			s := <-signal_chan
			switch s {
			case syscall.SIGHUP:
				aliases, err = fetchEmailAliases(*alias_url)
			}
		}

	}()

	periodic := time.NewTicker(time.Duration(*refresh_time) * time.Second)
	go func() {
		for {
			select {
			case <-periodic.C:
				syscall.Kill(syscall.Getpid(), syscall.SIGHUP)
			}
		}
	}()

	server := &smtpd.Server{

		Hostname: config.Host,

		Handler: func(peer smtpd.Peer, env smtpd.Envelope) error {
			for _, recipient := range env.Recipients {

				// get alias email source -> destination
				alias, err := getAlias(aliases, recipient)

				if err == nil {
					ix := strings.Index(alias.Destination, "@")
					domain := alias.Destination[ix+1:]
					servername := getMX(domain)

					if servername != "" {
						log.Println("received email for " + recipient + " and forwarding to " + alias.Destination + " via " + servername)
						mailhost := servername + ":smtp"
						smtpConn, connErr := net.Dial("tcp", mailhost)

						if connErr != nil {
							log.Println("connect error for "+mailhost, connErr)
							return connErr
						}

						client, smtpErr := smtp.NewClient(smtpConn, servername)
						if smtpErr != nil {
							log.Println("failed to create client for "+mailhost, smtpErr)
							return smtpErr
						}
						err = client.Mail(env.Sender)
						if err != nil {
							log.Println("mail-from error", err)
							return err
						}
						err = client.Rcpt(alias.Destination)
						if err != nil {
							log.Println("rcpt-to error", err)
							return err
						}

						data, writeErr := client.Data()

						_, writeErr = data.Write(env.Data)

						if writeErr != nil {
							log.Println("failed to write data to "+mailhost, writeErr)
							return writeErr
						}

						data.Close()
						client.Quit()
					}
				}

			}
			return nil
		},

		RecipientChecker: func(peer smtpd.Peer, addr string) error {
			return nil
		},

		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},

		ForceTLS: *force_tls,
	}

	server_bind := config.Bind + ":" + config.Port
	log.Println("listening on " + server_bind)

	err = server.ListenAndServe(server_bind)

	if err != nil {
		log.Fatal(err)
	}

	log.Println("terminating")
}
