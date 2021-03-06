package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"time"

	flapjack "github.com/flapjack/flapjack/src/flapjack"
)

type ApiClient struct {
	config  Config
	redis   flapjack.Transport
	http    *http.Transport
	request *http.Request
}

func (ac *ApiClient) Cancel() {
	ac.http.CancelRequest(ac.request)
}

func (ac *ApiClient) NewHttpClient() *http.Client {
	var tls_config *tls.Config

	if ac.config.IcingaCertfile != "" {
		// assuming self-signed server cert -- /etc/icinga2/ca.crt
		// TODO check behaviour for using system cert store (valid public cert)
		CA_Pool := x509.NewCertPool()
		serverCert, err := ioutil.ReadFile(ac.config.IcingaCertfile)
		if err != nil {
			log.Fatalln("Could not load server certificate")
		}
		CA_Pool.AppendCertsFromPEM(serverCert)

		tls_config = &tls.Config{RootCAs: CA_Pool}
	}

	var tr *http.Transport
	if tls_config == nil {
		tr = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   time.Duration(ac.config.IcingaTimeoutMS) * time.Millisecond,
				KeepAlive: time.Duration(ac.config.IcingaKeepAliveMS) * time.Millisecond,
			}).Dial,
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			TLSHandshakeTimeout: 10 * time.Second,
		}
		log.Println("Skipping verification of server TLS certificate")
	} else {
		tr = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   time.Duration(ac.config.IcingaTimeoutMS) * time.Millisecond,
				KeepAlive: time.Duration(ac.config.IcingaKeepAliveMS) * time.Millisecond,
			}).Dial,
			TLSClientConfig:     tls_config,
			TLSHandshakeTimeout: 10 * time.Second,
		}
	}
	client := &http.Client{
		Transport: tr,
	}

	ac.http = tr
	return client
}

func (ac *ApiClient) NewHttpRequest(method string, url string) *http.Request {
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Add("Accept", "application/json")
	req.SetBasicAuth(ac.config.IcingaUser, ac.config.IcingaPassword)
	return req
}

func (ac *ApiClient) Connect(finished chan<- error) {
	icinga_url_parts := []string{
		"https://", ac.config.IcingaServer, "/v1/events?queue=", ac.config.IcingaQueue,
		"&types=CheckResult&types=StateChange",
	}
	var icinga_url bytes.Buffer
	for i := range icinga_url_parts {
		icinga_url.WriteString(icinga_url_parts[i])
	}

	client := ac.NewHttpClient()
	req := ac.NewHttpRequest("POST", icinga_url.String())

	ac.request = req

	go func() {
		done := false

		for done == false {
			resp, err := client.Do(req)
			if err == nil {
				if ac.config.Debug {
					fmt.Printf("URL: %+v\n", icinga_url.String())
					fmt.Printf("Response: %+v\n", resp.Status)
				}

				if resp.StatusCode == http.StatusOK {
					err = ac.processResponse(resp)
				} else {
					defer func() {
						resp.Body.Close()
					}()
					body, _ := ioutil.ReadAll(resp.Body)
					err = fmt.Errorf("API HTTP request failed: %s , %s", resp.Status, body)
				}
			}

			if err != nil {
				finished <- err
				done = true
			}
		}
	}()

}

func (ac *ApiClient) processResponse(resp *http.Response) error {
	defer func() {
		// this makes sure that the HTTP connection will be re-used properly -- exhaust
		// stream and close the handle
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	decoder := json.NewDecoder(resp.Body)

	for decoder.More() {

		var data interface{}

		err := decoder.Decode(&data)

		if err != nil {
			return err
		}

		m := data.(map[string]interface{})

		if ac.config.Debug {
			fmt.Printf("Decoded Response: %+v\n", data)
		}

		switch m["type"] {
		case "CheckResult", "StateChange":
			check_result := m["check_result"].(map[string]interface{})
			timestamp := m["timestamp"].(float64)

			// https://github.com/Icinga/icinga2/blob/master/lib/icinga/checkresult.ti#L37-L48
			var state string

			switch check_result["state"].(float64) {
			case 0.0:
				state = "ok"
			case 1.0:
				state = "warning"
			case 2.0:
				state = "critical"
			case 3.0:
				state = "unknown"
			default:
			}

			if state == "" {
				return fmt.Errorf("Unknown state %.1f", check_result["state"].(float64))
			}

			// build and submit Flapjack redis event
			var varURL string
			var service string
			var serviceType string
			var name string

			if serv, ok := m["service"]; ok {
				service = serv.(string)
				serviceType = "services"
				name = fmt.Sprintf("%s!%s", m["host"], m["service"])
			} else {
				service = "HOST"
				serviceType = "hosts"
				name = m["host"].(string)
			}

			varURL = fmt.Sprintf("https://%s/v1/objects/%s/%s", ac.config.IcingaServer, serviceType, name)

			client := ac.NewHttpClient()
			req := ac.NewHttpRequest("GET", varURL)

			resp, _ = client.Do(req)
			decoder = json.NewDecoder(resp.Body)
			err = decoder.Decode(&data)
			if err != nil {
				return err
			}

			extra := data.(map[string]interface{})
			result := extra["results"].([]interface{})
			first := result[0].(map[string]interface{})
			attrs := first["attrs"].(map[string]interface{})
			vars := attrs["vars"].(map[string]interface{})

			var tags []string
			if val, ok := vars["tags"]; ok {
				tags = val.([]string)
			}

			event := flapjack.Event{
				Entity:  m["host"].(string),
				Check:   service,
				Type:    "service",
				Time:    int64(timestamp),
				State:   state,
				Summary: check_result["output"].(string),
				Details: fmt.Sprintf("tags: %s", tags),
				Tags:    tags,
			}

			// TODO handle err better -- e.g. redis down?
			_, err := ac.redis.SendVersionQueue(event, ac.config.FlapjackVersion, ac.config.FlapjackEvents)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("Unknown type %s", m["type"])
		}
	}
	return nil
}
