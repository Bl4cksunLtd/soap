package soap

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// UserAgent is the default user agent
const userAgent = "go-soap-1.1"

// XMLMarshaller lets you inject your favourite custom xml implementation
type XMLMarshaller interface {
	Marshal(v interface{}) ([]byte, error)
	Unmarshal(xml []byte, v interface{}) error
}

type defaultMarshaller struct{}

func (dm defaultMarshaller) Marshal(v interface{}) ([]byte, error) {
	return xml.MarshalIndent(v, "", "	")
}

func (dm defaultMarshaller) Unmarshal(xmlBytes []byte, v interface{}) error {
	return xml.Unmarshal(xmlBytes, v)
}

// BasicAuth credentials for the client
type BasicAuth struct {
	Login    string
	Password string
}

// Client generic SOAP client
type Client struct {
	Log             func(...interface{}) // optional
	url             string
	tls             bool
	auth            *BasicAuth
	Marshaller      XMLMarshaller
	UserAgent       string            // optional, falls back to "go-soap-0.1"
	ContentType     string            // optional, falls back to SOAP 1.1
	RequestHeaderFn func(http.Header) // optional, allows to modify the request header before it gets submitted.
	SoapVersion     string
	HTTPClientDoFn  func(req *http.Request) (*http.Response, error)
}

// NewClient constructor. SOAP 1.1 is used by default. Switch to SOAP 1.2 with
// UseSoap12(). Argument rt can be nil and it will fall back to the default
// http.Transport.
func NewClient(url string, auth *BasicAuth) *Client {
	return &Client{
		Log:            func(...interface{}) {}, // do nothing or add your fmt.Print* or log.*
		url:            url,
		auth:           auth,
		Marshaller:     defaultMarshaller{},
		ContentType:    SoapContentType11, // default is SOAP 1.1
		SoapVersion:    SoapVersion11,
		HTTPClientDoFn: http.DefaultClient.Do,
	}
}

func (c *Client) UseSoap11() {
	c.SoapVersion = SoapVersion11
	c.ContentType = SoapContentType11
}

func (c *Client) UseSoap12() {
	c.SoapVersion = SoapVersion12
	c.ContentType = SoapContentType12
}

// Call makes a SOAP call
func (c *Client) Call(ctx context.Context, soapAction string, request, response interface{}) (*http.Response, error) {
	envelope := Envelope{
		Body: Body{Content: request},
	}

	xmlBytes, err := c.Marshaller.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	// Adjust namespaces for SOAP 1.2
	if c.SoapVersion == SoapVersion12 {
		xmlBytes = replaceSoap11to12(xmlBytes)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(xmlBytes))
	if err != nil {
		return nil, err
	}
	if c.auth != nil {
		req.SetBasicAuth(c.auth.Login, c.auth.Password)
	}

	req.Header.Add("Content-Type", c.ContentType)
	ua := c.UserAgent
	if ua == "" {
		ua = userAgent
	}
	req.Header.Set("User-Agent", ua)

	if soapAction != "" {
		req.Header.Add("SOAPAction", soapAction)
	}

	req.Close = true
	if c.RequestHeaderFn != nil {
		c.RequestHeaderFn(req.Header)
	}
	c.Log("POST to", c.url, "with\n", xmlBytes)
	c.Log("Header", req.Header)
	httpResponse, err := c.HTTPClientDoFn(req)
	if err != nil {
		return nil, err
	}
	defer httpResponse.Body.Close()

	c.Log("\n\n## Response header:\n", httpResponse.Header)

	mediaType, params, err := mime.ParseMediaType(httpResponse.Header.Get("Content-Type"))
	if err != nil {
		c.Log("WARNING:", err)
	}
	c.Log("MIMETYPE:", mediaType)
	var rawBody []byte
	if strings.HasPrefix(mediaType, "multipart/") { // MULTIPART MESSAGE
		mr := multipart.NewReader(httpResponse.Body, params["boundary"])
		// If this is a multipart message, search for the soapy part
		foundSoap := false
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			slurp, err := ioutil.ReadAll(p)
			if err != nil {
				return nil, err
			}
			if bytes.HasPrefix(slurp, soapPrefixTagLC) || bytes.HasPrefix(slurp, soapPrefixTagUC) {
				rawBody = slurp
				foundSoap = true
				break
			}
		}
		if !foundSoap {
			return nil, errors.New("multipart message does contain a soapy part")
		}
	} else { // SINGLE PART MESSAGE
		rawBody, err = ioutil.ReadAll(httpResponse.Body)
		if err != nil {
			return httpResponse, err // return both
		}
		// Check if there is a body and if yes if it's a soapy one.
		if len(rawBody) == 0 {
			c.Log("INFO: Response Body is empty!")
			return httpResponse, nil // Empty responses are ok. Sometimes Sometimes only a Status 200 or 202 comes back
		}
		// There is a message body, but it's not SOAP. We cannot handle this!
		if !(bytes.Contains(rawBody, soapPrefixTagLC) || bytes.Contains(rawBody, soapPrefixTagUC)) {
			c.Log("This is not a SOAP-Message: \n", rawBody)
			return nil, errors.New("This is not a SOAP-Message: \n" + string(rawBody))
		}
		c.Log("RAWBODY\n", rawBody)
	}

	// We have an empty body or a SOAP body
	c.Log("\n\n## Response body:\n", rawBody)

	// Our structs for Envelope, Header, Body and Fault are tagged with namespace
	// for SOAP 1.1. Therefore we must adjust namespaces for incoming SOAP 1.2
	// messages
	rawBody = replaceSoap12to11(rawBody)

	respEnvelope := &Envelope{
		Body: Body{Content: response},
	}
	// Response struct may be nil, e.g. if only a Status 200 is expected. In this
	// case, we need a Dummy response to avoid a nil pointer if we receive a
	// SOAP-Fault instead of the empty message (unmarshalling would fail).
	if response == nil {
		respEnvelope.Body = Body{Content: &dummyContent{}} // must be a pointer in dummyContent
	}
	if err := xml.Unmarshal(rawBody, respEnvelope); err != nil {
		return nil, fmt.Errorf("soap/client.go Call(): COULD NOT UNMARSHAL: %s\n", err)
	}

	// If a SOAP Fault is received, try to jsonMarshal it and return it via the
	// error.
	if fault := respEnvelope.Body.Fault; fault != nil {
		return nil, errors.New("SOAP FAULT:\n" + formatFaultXML(rawBody, 1))
	}
	return httpResponse, nil
}

// Format the Soap Fault as indented string. Namespaces are dropped for better
// readability. Tags with lower level than start level is omitted.
func formatFaultXML(xmlBytes []byte, startLevel int) string {
	indent := "	"
	d := xml.NewDecoder(bytes.NewBuffer(xmlBytes))

	level := 0
	var out bytes.Buffer
	out.Grow(len(xmlBytes))
	ind := func() {
		n := 0
		if level-startLevel-1 > 0 {
			n = level - startLevel - 1
		}
		out.Write([]byte(strings.Repeat(indent, n)))
	}
	lf := func() {
		out.Write([]byte("\n"))
	}

	lastWasStart := false
	lastWasCharData := false
	lastWasEnd := false

	for token, err := d.Token(); token != nil && err == nil; token, err = d.Token() {
		switch tt := token.(type) {
		case xml.StartElement:
			lastWasCharData = false

			if lastWasEnd || lastWasStart {
				lf()
			}
			lastWasStart = true
			ind()
			elementName := tt.Name.Local

			if level > startLevel {
				out.WriteString("<" + elementName)
				out.WriteString(">")
			}

			level++
			lastWasEnd = false
		case xml.CharData:
			lastWasCharData = true
			_ = lastWasCharData
			lastWasStart = false

			xml.EscapeText(&out, tt)
			lastWasEnd = false
		case xml.EndElement:
			level--
			if lastWasEnd {
				lf()
				ind()
			}
			lastWasEnd = true
			lastWasStart = false

			if level > startLevel {
				endTagName := tt.Name.Local
				out.WriteString("</" + endTagName + ">")
			}

		}
	}
	return string(bytes.Trim(out.Bytes(), " \n"))
}

var (
	soapPrefixTagUC = []byte("<SOAP")
	soapPrefixTagLC = []byte("<soap")
)

func replaceSoap12to11(data []byte) []byte {
	return bytes.ReplaceAll(data, bNamespaceSoap12, bNamespaceSoap11)
}

func replaceSoap11to12(data []byte) []byte {
	return bytes.ReplaceAll(data, bNamespaceSoap11, bNamespaceSoap12)
}
