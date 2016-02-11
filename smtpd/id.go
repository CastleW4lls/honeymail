package smtpd

import (
	"crypto/sha1"
	"fmt"
	"time"
)

var (
	idGenerator = make(chan string)
)

func init() {

	go func() {
		h := sha1.New()
		c := []byte(time.Now().String())
		for {
			h.Write(c)
			id <- fmt.Sprintf("%x", h.Sum(nil))
		}
	}()

}
