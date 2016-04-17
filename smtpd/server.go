// This module creates the TCP SMTP daemon
package smtpd

import (
	"bufio"
	"crypto/tls"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/sec51/goconf"
	"github.com/sec51/honeymail/envelope"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"time"
)

var (
	// map which contains a mapping between the connection and the conversation between the server and the client
	clientConnections = make(map[string]*clientSession)
	// mutex needed to moodify the map
	clientMutex sync.Mutex

	// current amount of clients connected
	totalClientConnections = 0

	// max amount of clients
	maxClientConnections = goconf.AppConf.DefaultInt("smtp.max_client_connections", 64000)
)

type tcpServer struct {
	stopMutex       sync.Mutex
	localAddr       string
	localPort       string
	name            string
	withTLS         bool
	tlsConfig       *tls.Config
	envelopeChannel chan envelope.Envelope
	conn            *net.TCPListener
}

// this is the module responsible for setting up the communication channe
func NewTCPServer(ip, port, serverName, certPath, keyPath string, withTLS bool, envelopeChannel chan envelope.Envelope) (*tcpServer, error) {

	server := tcpServer{
		localAddr:       ip,
		localPort:       port,
		name:            serverName,
		withTLS:         withTLS,
		envelopeChannel: envelopeChannel,
	}

	if withTLS {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, err
		}
		server.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ServerName:   serverName}

	}

	return &server, nil

}

// this is a blocking call
func (s *tcpServer) Start() {

	addr, err := net.ResolveTCPAddr("tcp", s.localAddr+":"+s.localPort)

	if err != nil {
		log.Fatalln(err)
	}

	ln, err := net.ListenTCP("tcp", addr)

	if err != nil {
		log.Fatalln(err)
	}

	// assign the conn to stop the server
	s.stopMutex.Lock()
	s.conn = ln
	s.stopMutex.Unlock()

	log.Infof("Honeymail server is listening on %s:%s", s.localAddr, s.localPort)

	for {
		if conn, err := ln.AcceptTCP(); err == nil {
			// we accept a maximum of 6400 concurrent connections
			// each agent creates 1 connection, therefore it should be enough for handling up to 6400 agents
			clientMutex.Lock()
			if totalClientConnections >= maxClientConnections {
				log.Errorln("Too many connections from mail clients. Stopped accepting new connections.")
				continue
			}
			clientMutex.Unlock()

			// otherwise accept the connection
			log.Infoln("Amount of mail client connections:", totalClientConnections)

			// set a read timeout
			conn.SetReadDeadline(time.Now().Add(4 * time.Minute))
			go s.handleTCPConnection(NewClientSession(conn))

		}
	}
}

func (s *tcpServer) Stop() error {
	s.stopMutex.Lock()
	defer s.stopMutex.Unlock()
	return s.conn.Close()
}

// Handles incoming requests.
// withTLS means the client connected directly with TLS
// this means you need to create two TCP server objects.
// one which listen to the TLS port wanted
func (s *tcpServer) handleTCPConnection(client *clientSession) {

	// Count the amount of errors and if it exceeds a threshold close the connection
	totalCommandErrors := 0

	// close the connection in case this exists
	defer client.conn.Close()

	// get the client remote address
	clientId := client.conn.RemoteAddr().String()

	// write the welcome message to the client
	if strings.Contains(kGreeting, "%s") {
		kGreeting = fmt.Sprintf(kGreeting, domainName)
	}
	if err := client.writeData(kGreeting); err != nil {
		log.Println("Error writing greeting message to mail client", err)
		return
	}

	// increment connection counter
	s.incrementConnectionCounter(clientId, client)

	// new mail client connection was successfully created
	// create a new envelope because we expect the client to send the HELO/EHLO command
	envelopeData := envelope.NewEnvelope(clientId)

	// new buffered reader
	bufferedReader := bufio.NewReader(client.conn)
	reader := textproto.NewReader(bufferedReader)

	// parsed command
	var command ParsedCommand

command_loop:
	for {

		// we are receiving the email's data
		// at this stage there should be not commands, just data.
		// therefore it needs to happen before the ReadLine (used to read a command)
		if client.isInDataMode() {
			// check if the message ends and read all buffer
			// if the message does not end, continue reading
			dotBytes, err := reader.ReadDotBytes()

			if err != nil {
				break
			}
			// means the message ends
			if err == nil && len(dotBytes) > 0 {

				// assign the data read to the mailData struct
				envelopeData.Message = dotBytes

				// write back to the client that the data part succeeded
				client.writeData(fmt.Sprintf(kMessageAccepted, envelopeData.Id))

				// set the state as post data, so during the loop it does not enter here again
				client.MarkInPostDataMode()

				// queue the envelope for processing
				// at this stage the client is allowed only to RSET or to QUIT
				// dereference the envelope and send it
				s.queueForDelivery(*envelopeData)

				// continue the loop
				continue
			}

			// continue reading
			continue
		}

		// read the command sent from the client, which is in the buffer
		line, err := reader.ReadLine()
		log.Infof("%s: %s", clientId, line)

		// parse the command line
		command = *ParseCmd(line)
		if command.Response != "" {
			log.Errorln("CAUGHT error while parsing the command", err, line)

			client.writeData(command.Response)

			// count the amount of total command errors
			totalCommandErrors = totalCommandErrors + 1

			// close connection in case of 5 consecutive errors
			if totalCommandErrors == 5 {
				break command_loop
			}

			continue
		}

		// verify that it's a valid command in the sequence
		// if it's not valid then answer and wait for a different command (continue)
		if err := client.verifyState(command.Cmd); err != nil {
			client.writeData(kBadCommandSequence)
			continue
		}

		// mark the state
		client.markState(command.Cmd)

		switch command.Cmd {
		case EXPN:
			client.writeData(kCommandNotImplemented)
			continue
		case HELP:
			client.writeData("214 SMTP servers help those who help themselves.")
			client.writeData("214 Go read http://cr.yp.to/smtp.html.")
			break
		case NOOP:
			client.writeData(kNoopCommand)
			break
		case VRFY:
			client.writeData(kVerifyAddress)
			break
		case RSET:
			// reset the envelopeData
			envelopeData = nil
			envelopeData = envelope.NewEnvelope(clientId)

			// resent the client state for the sequence of commands
			client.reset()
			client.writeData("250 OK")
			break
		case STARTTLS:
			// Init a new TLS connection. I need a *tls.Conn type
			// so that I can do the Handshake()
			var tlsConn *tls.Conn
			tlsConn = tls.Server(client.conn, s.tlsConfig)

			// Here is the trick. Since I do not need to access
			// any of the TLS functions anymore,
			// I can convert tlsConn back in to a net.Conn type
			client.tlsConn = tlsConn

			// Reset the buffered reader
			bufferedReader = bufio.NewReader(client.tlsConn)
			reader = textproto.NewReader(bufferedReader)

			// run a handshake
			// Verify on the RFC what the server is supposed to do when the TLS handshake fails
			err := tlsConn.Handshake()
			if err != nil {
				log.Errorln("Failed to handshake with the client a valid SSL connection")
				client.writeData(kClosingConnection)
				break command_loop
			}

			client.isTLS = true

			// mark the envelopeData as securely delivered (we should check whether the STARTTLS command was issued before the MAIL FROM)
			if !client.hasInitiatedMailTransaction() {
				envelopeData.SecurelyDelivered = true
			}

			// defer closing of the connection
			defer client.tlsConn.Close()

			break
		case HELO:
			if err := client.verifyHost(command.Argument); err != nil {
				log.Warnf("Suspicious connection from: %s; continuing nonetheless", clientId)
			}
			client.writeData(fmt.Sprintf("250 %s Hello %v", s.name, client.remoteAddress))
			break
		case EHLO:
			if err := client.verifyHost(command.Argument); err != nil {
				log.Warnf("Suspicious connection from: %s; continuing nonetheless", clientId)
			}

			client.writeData(fmt.Sprintf("250-%s Hello %v", s.name, client.remoteAddress))
			client.writeData(fmt.Sprintf("250-SIZE %d", kFixedSize))
			//client.writeData("250-ENHANCEDSTATUSCODES")
			client.writeData("250-PIPELINING")
			client.writeData("250 8BITMIME")
			client.writeData("250-VRFY")
			client.writeData("250-HELP")

			// we cannot advertise STARTTLS in case it was already
			// or in case the connection happens already via TLS
			if !s.withTLS && !client.isTLS {
				client.writeData("250-STARTTLS")
			}

			// TODO: implement AUTH

		case AUTH:
			client.writeData(kCommandNotImplemented)
			break
		case MAILFROM:

			// parse the mail address and make sure it'a a valid one
			fromAddress, err := verifyEmailAddress(command.Argument)
			if err != nil {
				log.Println("Error parsing FROM address", err)
				client.writeData(kRequestAborted)
				continue
			}
			envelopeData.From = fromAddress
			client.writeData(kRecipientAccepted)
			break
		case RCPTTO:

			// parse the mail address and make sure it'a a valid one
			toAddress, err := verifyEmailAddress(command.Argument)
			if err != nil {
				log.Println("Error parsing TO address", err)
				client.writeData(kRequestAborted)
				continue
			}

			// the first add it to the TO the following to the forward
			if envelopeData.To == nil {
				envelopeData.To = toAddress
			} else {
				envelopeData.AddForward(toAddress)
			}

			client.writeData(kRecipientAccepted)
			break
		case DATA:
			client.writeData(kSendData)
			break
		case QUIT:
			client.writeData(kClosingConnection)
			break command_loop
		default:
			if client.needsToQuit() {
				break command_loop
			}
		}

	}

	// at this point the connection will be closed therefore decrease the counter
	s.decrementConnectionCounter(clientId)
	log.Infoln("Client", clientId, "disconnected")

}

func (s *tcpServer) incrementConnectionCounter(clientId string, client *clientSession) {

	// update the map and the total connections
	clientMutex.Lock()
	totalClientConnections++
	clientConnections[clientId] = client
	clientMutex.Unlock()

}

func (s *tcpServer) decrementConnectionCounter(clientId string) {
	clientMutex.Lock()
	totalClientConnections--
	delete(clientConnections, clientId)
	clientMutex.Unlock()
}

func (s *tcpServer) queueForDelivery(e envelope.Envelope) {
	if s.envelopeChannel != nil {
		s.envelopeChannel <- e
	}

}

func verifyEmailAddress(email string) (*mail.Address, error) {
	return mail.ParseAddress(email)
}
