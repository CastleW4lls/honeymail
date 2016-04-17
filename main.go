package main

import (
	//"flag"
	log "github.com/Sirupsen/logrus"
	"github.com/boltdb/bolt"
	"github.com/sec51/goconf"
	"github.com/sec51/honeymail/api"
	"github.com/sec51/honeymail/envelope"
	"github.com/sec51/honeymail/geoip"
	"github.com/sec51/honeymail/processor"
	"github.com/sec51/honeymail/smtpd"
	"github.com/sec51/honeymail/storage"
)

// Flags
// var (
// 	dbPath      = flag.String("dbPath", "GeoLite2-City.mmdb", "Full path of the Geo Lite City mmdb file")
// 	ip          = flag.String("ip", "0.0.0.0", "Listen on this address")
// 	serverName  = flag.String("serverName", "localhost", "Server name to expose to the world during the hello handshake")
// 	smtpPort    = flag.String("smtpPort", "10025", "Standard SMTP PORT")
// 	smtpPortTLS = flag.String("smtpPortTLS", "587", "TLS smtp port for submission")
// 	certificate = flag.String("certificate", "", "TLS public certificate")
// 	privateKey  = flag.String("privateKey", "", "TLS private key")
// )

func main() {

	// define configurations
	dbPath := goconf.AppConf.DefaultString("maxmind.db.path", "GeoLite2-City.mmdb")
	ip := goconf.AppConf.DefaultString("smtp.listen_to", "0.0.0.0")
	serverName := goconf.AppConf.DefaultString("smtp.server_name", "localhost")
	smtpPort := goconf.AppConf.DefaultString("smtp.port", "10025")
	smtpSecurePort := goconf.AppConf.DefaultString("smtp.secure_port", "10026")
	certificate := goconf.AppConf.DefaultString("smtp.tls.public_key", "")
	privateKey := goconf.AppConf.DefaultString("smtp.tls.private_key", "")

	apiHost := goconf.AppConf.DefaultString("http.listen_to", "0.0.0.0")
	apiPort := goconf.AppConf.DefaultString("http.port", "8080")

	// ===========================

	// DB STORAGE for emails
	db, err := bolt.Open("mail.db", 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// GeoIP Resolution
	err = geoip.InitGeoDb(dbPath)
	if err != nil {
		log.Fatal(err)
	}

	defer geoip.Resolver().Close()

	// ==========================================

	// channel for processing the envelopes
	envelopeChannel := make(chan envelope.Envelope)

	// channel for storing the envelopes
	storageChannel := make(chan envelope.Envelope)

	// ============================
	// Storage service
	storageService := storage.NewStorageService(db, storageChannel)
	storageService.Start()

	// ============================
	// Processing service - caluclates the stats and extract additional info from each envelope
	// the passes it onto the storage channel for storing the results
	processorService := processor.NewProcessorService(envelopeChannel, storageChannel)
	processorService.Start()

	// DEBUG ONLY
	if todayEmails, err := storageService.ViewTodayEnvelopes(); err == nil {
		for _, envelope := range todayEmails {
			log.Infof("Id: %s => From: %s => To: %s\n%s", envelope.Id, envelope.From.String(), envelope.To.String(), envelope.Message)
		}
	}
	// ============================

	withTLS := certificate != "" && privateKey != ""
	server, err := smtpd.NewTCPServer(ip, smtpPort, smtpSecurePort, serverName, certificate, privateKey, withTLS, envelopeChannel)
	if err != nil {
		log.Fatal(err)
	}

	go server.Start()

	// API
	apiService := api.NewAPIService(apiHost, apiPort, storageService)
	apiService.Start()
}
