package empi

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"

	"github.com/google/uuid"

	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/gorilla/mux"
	"github.com/patrickmn/go-cache"
)

// Endpoint represents a specific SOAP server providing access to "enterprise master patient index" (EMPI) data
type Endpoint int

// A list of endpoints
const (
	UnknownEndpoint     Endpoint = iota // unknown
	ProductionEndpoint                  // production server
	TestingEndpoint                     // user acceptance testing
	DevelopmentEndpoint                 // development
)

var endpointURLs = [...]string{
	"",
	"https://mpilivequeries.cymru.nhs.uk/PatientDemographicsQueryWS.asmx",
	"https://mpitest.cymru.nhs.uk/PatientDemographicsQueryWS.asmx",
	"http://ndc06srvmpidev2.cymru.nhs.uk:23000/PatientDemographicsQueryWS.asmx",
}

var endpointNames = [...]string{
	"unknown",
	"production",
	"testing",
	"development",
}

var endpointCodes = [...]string{
	"",
	"P",
	"U",
	"T",
}

// LookupEndpoint returns an endpoint for (P)roduction, (T)esting or (D)evelopment
func LookupEndpoint(s string) Endpoint {
	s2 := strings.ToUpper(s)
	switch {
	case strings.HasPrefix(s2, "P"):
		return ProductionEndpoint
	case strings.HasPrefix(s2, "T"):
		return TestingEndpoint
	case strings.HasPrefix(s2, "D"):
		return DevelopmentEndpoint
	}
	return UnknownEndpoint
}

// URL returns the default URL of this endpoint
func (ep Endpoint) URL() string {
	return endpointURLs[ep]
}

// ProcessingID returns the processing ID for this endpoint
func (ep Endpoint) ProcessingID() string {
	return endpointCodes[ep]
}

// Name returns the name of this endpoint
func (ep Endpoint) Name() string {
	return endpointNames[ep]
}

var endpoint = flag.String("endpoint", "D", "(P)roduction, (T)esting or (D)evelopment")
var authority = flag.String("authority", "NHS", "Authority, such as NHS, 140 (CAV) etc")
var identifier = flag.String("id", "", "identifier to fetch e.g. 7253698428, 7705820730, 6145933267")
var logger = flag.String("log", "", "logfile to use")
var serve = flag.Bool("serve", false, "whether to start a REST server")
var port = flag.Int("port", 8080, "port to use")
var cacheMinutes = flag.Int("cache", 5, "cache expiration in minutes, 0=no cache")
var fake = flag.Bool("fake", false, "run a fake service")
var timeoutSeconds = flag.Int("timeout", 2, "timeout in seconds for external services")

// unset http_proxy
// unset https_proxy
func main2() {
	flag.Parse()
	if *logger != "" {
		f, err := os.OpenFile(*logger, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
		if err != nil {
			log.Fatalf("fatal error: couldn't open log file ('%s'): %s", *logger, err)
		}
		log.SetOutput(f)
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}
	httpProxy, exists := os.LookupEnv("http_proxy") // give warning if proxy set, to help debug connection errors in live
	if exists {
		log.Printf("warning: http proxy set to %s\n", httpProxy)
	}
	httpsProxy, exists := os.LookupEnv("https_proxy")
	if exists {
		log.Printf("warning: https proxy set to %s\n", httpsProxy)
	}
	ep := LookupEndpoint(*endpoint)
	if endpointURLs[ep] == "" {
		log.Fatalf("error: unknown or unsupported endpoint: %s", *endpoint)
	}

	// handle a command-line test with a specified identifier
	if *identifier != "" {
		ctx := context.Background()
		auth := LookupAuthority(*authority)
		if auth == AuthorityUnknown {
			log.Fatalf("unsupported authority: %s", *authority)
		}
		pt, err := performRequest(ctx, endpointURLs[ep], endpointCodes[ep], auth, *identifier)
		if err != nil {
			log.Fatal(err)
		}
		if pt == nil {
			log.Printf("Not Found")
			return
		}
		if err := json.NewEncoder(os.Stdout).Encode(pt); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *serve {
		sigs := make(chan os.Signal, 1) // channel to receive OS termination/kill/interrupt signal
		signal.Notify(sigs, os.Interrupt, os.Kill, syscall.SIGTERM)
		go func() {
			s := <-sigs
			log.Printf("RECEIVED SIGNAL: %s", s)
			os.Exit(1)
		}()
		app := new(App)
		app.Endpoint = ep
		app.Router = mux.NewRouter().StrictSlash(true)
		app.Fake = *fake
		app.TimeoutSeconds = *timeoutSeconds
		if *cacheMinutes != 0 {
			app.Cache = cache.New(time.Duration(*cacheMinutes)*time.Minute, time.Duration(*cacheMinutes*2)*time.Minute)
		}
		app.Router.HandleFunc("/nhsnumber/{nnn}", app.GetByNhsNumber).Methods("GET")
		app.Router.HandleFunc("/authority/{authorityCode}/{identifier}", app.GetByIdentifier).Methods("GET")
		log.Printf("starting REST server: port:%d cache:%dm timeout:%ds endpoint:(%s)%s",
			*port, *cacheMinutes, *timeoutSeconds, endpointNames[ep], endpointURLs[ep])
		log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), app.Router))
		return
	}
	flag.PrintDefaults()
}

// Invoke invokes a simple request on the endpoint for the specified authority and identifier
func Invoke(endpointURL string, processingID string, authority string, identifier string) {
	ctx := context.Background()
	auth := LookupAuthority(authority)
	if auth == AuthorityUnknown {
		log.Fatalf("unsupported authority: %s", authority)
	}
	pt, err := performRequest(ctx, endpointURL, processingID, auth, identifier)
	if err != nil {
		log.Fatal(err)
	}
	if pt == nil {
		log.Printf("Not Found")
		return
	}
	if err := json.NewEncoder(os.Stdout).Encode(pt); err != nil {
		log.Fatal(err)
	}
}

// App represents the application
type App struct {
	Endpoint       Endpoint
	Router         *mux.Router
	Cache          *cache.Cache // may be nil if not caching
	Fake           bool
	TimeoutSeconds int
}

func (a *App) getCache(key string) (*Patient, bool) {
	if a.Cache == nil {
		return nil, false
	}
	if o, found := a.Cache.Get(key); found {
		return o.(*Patient), true
	}
	return nil, false
}

func (a *App) setCache(key string, value *Patient) {
	if a.Cache == nil {
		return
	}
	a.Cache.Set(key, value, cache.DefaultExpiration)
}

func (a *App) GetByNhsNumber(w http.ResponseWriter, r *http.Request) {
	nnn := mux.Vars(r)["nnn"]
	query := r.URL.Query()
	user := query.Get("user")
	log.Printf("request by user: '%s' for nnn: '%s': %+v", user, nnn, r)
	if user == "" {
		log.Printf("bad request: invalid user")
		http.Error(w, "invalid user", http.StatusBadRequest)
		return
	}
	if nnn == "" || len(nnn) != 10 {
		log.Printf("bad request: invalid NHS number")
		http.Error(w, "invalid nhs number", http.StatusBadRequest)
		return
	}
	a.writeIdentifier(w, r, authorityCodes[AuthorityNHS], nnn, user)
}

func (a *App) GetByIdentifier(w http.ResponseWriter, r *http.Request) {
	authority := mux.Vars(r)["authorityCode"]
	identifier := mux.Vars(r)["identifier"]
	query := r.URL.Query()
	user := query.Get("user")
	log.Printf("request by user:%s for authority:%s id:%s: %+v", user, authority, identifier, r)
	if user == "" {
		log.Print("bad request: invalid user")
		http.Error(w, "invalid user", http.StatusBadRequest)
		return
	}
	if LookupAuthority(authority) == AuthorityUnknown {
		log.Printf("bad request: unknown authority: %s", authority)
		http.Error(w, "invalid authority", http.StatusBadRequest)
		return
	}
	a.writeIdentifier(w, r, authority, identifier, user)
}

func (a *App) writeIdentifier(w http.ResponseWriter, r *http.Request, authority string, identifier string, username string) {
	start := time.Now()
	key := authority + "/" + identifier
	pt, found := a.getCache(key)
	var err error
	if !found {
		if !a.Fake {
			ctx, cancelFunc := context.WithTimeout(context.Background(), time.Duration(a.TimeoutSeconds)*time.Second)
			pt, err = performRequest(ctx, endpointURLs[a.Endpoint], endpointCodes[a.Endpoint], LookupAuthority(authority), identifier)
			cancelFunc()
		} else {
			pt, err = performFake(LookupAuthority(authority), identifier)
		}
		if err != nil {
			log.Printf("error: %s", err)
			if urlError, ok := err.(*url.Error); ok {
				if urlError.Timeout() {
					http.Error(w, err.Error(), http.StatusRequestTimeout)
					return
				}
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.setCache(key, pt)
	} else {
		log.Printf("serving request for %s/%s from cache in %s", authority, identifier, time.Since(start))
	}
	if pt == nil {
		log.Printf("patient with identifier %s/%s not found", authority, identifier)
		http.NotFound(w, r)
		return
	}
	log.Printf("result: %+v", pt)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if err := json.NewEncoder(w).Encode(pt); err != nil {
		log.Printf("error: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func performFake(authority Authority, identifier string) (*Patient, error) {
	dob := time.Date(1960, 01, 01, 00, 00, 00, 0, time.UTC)

	return &Patient{
		Lastname:            "DUMMY",
		Firstnames:          "ALBERT",
		Title:               "DR",
		Gender:              "M",
		BirthDate:           &dob,
		Surgery:             "W95010",
		GeneralPractitioner: "G9342400",
		Identifiers: []Identifier{
			Identifier{
				System: authorityCodes[authority],
				Value:  identifier,
			},
			Identifier{
				System: "103",
				Value:  "M1147907",
			},
		},
		Addresses: []Address{
			Address{
				Text:       "59 Robins Hill\nBrackla\nBRIDGEND\nCF31 2PJ\nWALES",
				Line:       "59 Robins Hill",
				City:       "Brackla",
				District:   "BRIDGEND",
				PostalCode: "CF31 2PJ",
				Country:    "WALES",
			},
		},
		Telecom: []ContactPoint{
			ContactPoint{
				System:      "phone",
				Value:       "02920747747",
				Use:         "work",
				Rank:        1,
				Description: "Work number",
			},
			ContactPoint{
				System:      "email",
				Value:       "test@test.com",
				Use:         "work",
				Rank:        1,
				Description: "Work email",
			},
		},
	}, nil
}

func performRequest(context context.Context, endpointURL string, processingID string, authority Authority, identifier string) (*Patient, error) {
	start := time.Now()
	data, err := NewIdentifierRequest(identifier, authority, "221", "100", processingID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(context, "POST", endpointURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-type", "text/xml; charset=\"utf-8\"")
	req.Header.Set("SOAPAction", "http://apps.wales.nhs.uk/mpi/InvokePatientDemographicsQuery")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var e envelope
	log.Printf("response (%s): %v", time.Since(start), string(body))
	err = xml.Unmarshal(body, &e)
	if err != nil {
		return nil, err
	}
	return e.ToPatient()
}

// IdentifierRequest is used to populate the template to make the XML request
type IdentifierRequest struct {
	Identifier           string
	Authority            string
	AuthorityType        string
	SendingApplication   string
	SendingFacility      string
	ReceivingApplication string
	ReceivingFacility    string
	DateTime             string
	MessageControlID     string //for MSH.10 -  a UUID
	ProcessingID         string //for MSH.11 - P/U/T production/testing/development
}

// NewIdentifierRequest returns a correctly formatted XML request to search by an identifier, such as NHS number
// sender : 221 (PatientCare)
// receiver: 100 (NHS Wales EMPI)
func NewIdentifierRequest(identifier string, authority Authority, sender string, receiver string, processingID string) ([]byte, error) {
	layout := "20060102150405" // YYYYMMDDHHMMSS
	now := time.Now().Format(layout)
	data := IdentifierRequest{
		Identifier:           identifier,
		Authority:            authorityCodes[authority],
		AuthorityType:        authorityTypes[authority],
		SendingApplication:   sender,
		SendingFacility:      sender,
		ReceivingApplication: receiver,
		ReceivingFacility:    receiver,
		DateTime:             now,
		MessageControlID:     uuid.New().String(),
		ProcessingID:         processingID,
	}
	t, err := template.New("identifier-request").Parse(identifierRequestTemplate)
	if err != nil {
		return nil, err
	}
	log.Printf("request: %+v", data)
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Authority represents the different authorities that issue identifiers
type Authority int

// List of authority codes for different organisations in Wales
const (
	AuthorityUnknown = iota
	AuthorityNHS
	AuthorityEMPI
	AuthorityABH
	AuthorityABMU
	AuthorityBCUCentral
	AuthorityBCUMaelor
	AuthorityBCUWest
	AuthorityCT
	AuthorityCV
	AuthorityHD
	AuthorityPowys
)

var authorityCodes = [...]string{
	"",
	"NHS", // NHS number
	"100", // internal EMPI identifier - ephemeral identifier
	"139", // AB
	"108", // ABM
	"109", //BCUCentral
	"110", //BCUMaelor
	"111", //BCUWest
	"126", //CT
	"140", //CAV
	"149", //HD
	"170", //Powys
}
var authorityTypes = [...]string{
	"",
	"NH",
	"PE", // unknown - TODO: check this
	"PI",
	"PI",
	"PI",
	"PI",
	"PI",
	"PI",
	"PI",
	"PI",
	"PI",
}

// LookupAuthority looks up an authority via a code
func LookupAuthority(authority string) Authority {
	for i, a := range authorityCodes {
		if a == authority {
			return Authority(i)
		}
	}
	return AuthorityUnknown
}

// Coding is a reference to a code in a coding system
type Coding struct {
	System       string `json:"system"`
	Version      string `json:"version"`
	Code         string `json:"code"`
	Display      string `json:"display"`
	UserSelected bool   `json:"userSelected"`
}

// CodeableConcept reflects one or more specific codes from a code system
type CodeableConcept struct {
	Coding []Coding `json:"coding"`
	Text   string   `json:"text"`
}

// Period reflects a time period
type Period struct {
	Start *time.Time `json:"start"`
	End   *time.Time `json:"end"`
}

// Reference allows one resource to reference another
type Reference struct {
	Reference  string     `json:"reference"`  // Literal reference, absolute or relative URL
	Type       string     `json:"type"`       // Type reference refers to, eg. Patient, Organization
	Identifier Identifier `json:"identifier"` // Logical reference when literal reference not known
	Display    string     `json:"display"`    // Text alternative for the resource
}

// Identifier represents an organisation's identifier for this patient
type Identifier struct {
	Use      string           `json:"use,omitempty"` // usual / official / temp / secondary / old  (if known)
	Type     *CodeableConcept `json:"type"`
	System   string           `json:"system"` // uri
	Value    string           `json:"value"`
	Period   *Period          `json:"period,omitempty"`
	Assigner *Reference       `json:"assigner"`
}

// Address represents an address for this patient
type Address struct {
	Use        string  `json:"use,omitempty"`
	Type       string  `json:"type,omitempty"`
	Text       string  `json:"text"`
	Line       string  `json:"line"`
	City       string  `json:"city"`
	District   string  `json:"district"`
	State      string  `json:"state"`
	PostalCode string  `json:"postalCode"`
	Country    string  `json:"country"`
	Period     *Period `json:"period"`
}

// Patient is a patient
type Patient struct {
	Lastname            string         `json:"lastName"`
	Firstnames          string         `json:"firstNames"`
	Title               string         `json:"title"`
	Gender              string         `json:"gender"`
	BirthDate           *time.Time     `json:"dateBirth"`
	DeathDate           *time.Time     `json:"dateDeath"`
	Surgery             string         `json:"surgery"`
	GeneralPractitioner string         `json:"generalPractitioner"`
	Identifiers         []Identifier   `json:"identifiers"`
	Addresses           []Address      `json:"addresses"`
	Telecom             []ContactPoint `json:"telecom"`
}

// ContactPoint is a technology-mediated contact point for a person or organization, including telephone, email,
type ContactPoint struct {
	System      string  `json:"system"` // phone | fax | email | pager | url | sms | other
	Value       string  `json:"value"`
	Use         string  `json:"use"` // home | work | temp | old | mobile - purpose of this contact point
	Rank        int     `json:"rank"`
	Period      *Period `json:"period,omitempty"`
	Description string  `json:"description"` // not standard - textual description
}

// ToPatient creates a "Patient" from the XML returned from the EMPI service
func (e *envelope) ToPatient() (*Patient, error) {
	pt := new(Patient)
	pt.Lastname = e.surname()
	pt.Firstnames = e.firstnames()
	if pt.Lastname == "" && pt.Firstnames == "" {
		return nil, nil
	}
	pt.Title = e.title()
	pt.Gender = e.gender()
	pt.BirthDate = e.dateBirth()
	pt.DeathDate = e.dateDeath()
	pt.Identifiers = e.identifiers()
	pt.Addresses = e.addresses()
	pt.Surgery = e.surgery()
	pt.GeneralPractitioner = e.generalPractitioner()
	pt.Telecom = e.telecom()
	return pt, nil
}

func (e *envelope) surname() string {
	names := e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID5
	if len(names) > 0 {
		return names[0].XPN1.FN1.Text
	}
	return ""
}

func (e *envelope) firstnames() string {
	names := e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID5
	var sb strings.Builder
	if len(names) > 0 {
		sb.WriteString(names[0].XPN2.Text) // given name - XPN.2
		sb.WriteString(" ")
		sb.WriteString(names[0].XPN3.Text) // second or further given names - XPN.3
	}
	return strings.TrimSpace(sb.String())
}

func (e *envelope) title() string {
	names := e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID5
	if len(names) > 0 {
		return names[0].XPN5.Text
	}
	return ""
}

func (e *envelope) gender() string {
	return e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID8.Text
}

func (e *envelope) dateBirth() *time.Time {
	dob := e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID7.TS1.Text
	if len(dob) > 0 {
		d, err := parseDate(dob)
		if err == nil {
			return d
		}
	}
	return nil
}

func (e *envelope) dateDeath() *time.Time {
	dod := e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID29.TS1.Text
	if len(dod) > 0 {
		d, err := parseDate(dod)
		if err == nil {
			return d
		}
	}
	return nil
}

func (e *envelope) surgery() string {
	return e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PD1.PD13.XON3.Text
}

func (e *envelope) generalPractitioner() string {
	return e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PD1.PD14.XCN1.Text
}

func (e *envelope) identifiers() []Identifier {
	result := make([]Identifier, 0)
	ids := e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID3
	for _, id := range ids {
		authority := id.CX4.HD1.Text
		identifier := id.CX1.Text
		if authority != "" && identifier != "" {
			result = append(result, Identifier{
				Use:    "official",
				System: authority,
				Assigner: &Reference{
					Reference: authority,
					Display:   authority, // todo: change to human readable name
				},
				Value: identifier,
			})
		}
	}
	return result
}

func (e *envelope) addresses() []Address {
	result := make([]Address, 0)
	addresses := e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID11
	for _, address := range addresses {
		dateFrom, _ := parseDate(address.XAD13.Text)
		dateTo, _ := parseDate(address.XAD14.Text)
		result = append(result, Address{
			Line:       address.XAD1.SAD1.Text,
			City:       address.XAD2.Text,
			District:   address.XAD3.Text,
			Country:    address.XAD4.Text,
			PostalCode: address.XAD5.Text,
			Period: &Period{
				Start: dateFrom,
				End:   dateTo,
			},
		})
	}
	return result
}

func (e *envelope) telecom() []ContactPoint {
	result := make([]ContactPoint, 0)
	pid13 := e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID13
	for _, telephone := range pid13 {
		num := telephone.XTN1.Text
		if num != "" {
			result = append(result, ContactPoint{
				System:      "phone",
				Value:       num,
				Description: telephone.LongName,
			})
		}
		email := telephone.XTN4.Text
		if email != "" {
			result = append(result, ContactPoint{
				System: "email",
				Value:  email,
			})
		}
	}
	pid14 := e.Body.InvokePatientDemographicsQueryResponse.RSPK21.RSPK21QUERYRESPONSE.PID.PID14
	for _, telephone := range pid14 {
		num := telephone.XTN1.Text
		if num != "" {
			result = append(result, ContactPoint{
				System:      "phone",
				Value:       num,
				Description: telephone.LongName,
			})
		}
		email := telephone.XTN4.Text
		if email != "" {
			result = append(result, ContactPoint{
				System: "email",
				Value:  email,
			})
		}
	}
	return result
}

func parseDate(d string) (*time.Time, error) {
	layout := "20060102" // reference date is : Mon Jan 2 15:04:05 MST 2006
	if len(d) > 8 {
		d = d[:8]
	}
	t, err := time.Parse(layout, d)
	if err != nil || t.IsZero() {
		return nil, err
	}
	return &t, nil
}

var identifierRequestTemplate = `
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:mpi="http://apps.wales.nhs.uk/mpi/" xmlns="urn:hl7-org:v2xml">
<soapenv:Header/>
<soapenv:Body>
   <mpi:InvokePatientDemographicsQuery>

	  <QBP_Q21>

		 <MSH>
			 <!--Field Separator -->
			<MSH.1>|</MSH.1>
			<!-- Encoding Characters -->
			<MSH.2>^~\&amp;</MSH.2>
			<!-- Sending Application -->
			<MSH.3 >
			   <HD.1>{{.SendingApplication}}</HD.1>
			</MSH.3>
			<!-- Sending Facility -->
			<MSH.4 >
			   <HD.1>{{.SendingFacility}}</HD.1>
			</MSH.4>
			<!-- Receiving Application -->
			<MSH.5>
			   <HD.1>{{.ReceivingApplication}}</HD.1>
			</MSH.5>
			<!-- Receiving Application -->
			<MSH.6>
			   <HD.1>{{.ReceivingFacility}}</HD.1>
			</MSH.6>
			<!-- Date / Time of message YYYYMMDDHHMMSS -->
			<MSH.7>
			   <TS.1>{{.DateTime}}</TS.1>
			</MSH.7>
			<!-- Message Type -->
			<MSH.9>
			   <MSG.1 >QBP</MSG.1>
			   <MSG.2 >Q22</MSG.2>
			   <MSG.3 >QBP_Q21</MSG.3>
			</MSH.9>
			<!-- Message Control ID -->
			<MSH.10>{{.MessageControlID}}</MSH.10>
			<MSH.11>
			   <PT.1 >{{.ProcessingID}}</PT.1>
			</MSH.11>
			<!-- Version Id -->
			<MSH.12>
			   <VID.1 >2.5</VID.1>
			</MSH.12>
			<!-- Country Code -->
			<MSH.17 >GBR</MSH.17>
		 </MSH>

		 <QPD>
			<QPD.1 >
			   <!--Message Query Name :-->
			   <CE.1>IHE PDQ Query</CE.1>
			</QPD.1>
			<!--Query Tag:-->
			<QPD.2>PatientQuery</QPD.2>
		  <!--Demographic Fields:-->
			<!--Zero or more repetitions:-->
			<QPD.3>
			   <!--PID.3 - Patient Identifier List:-->
			   <QIP.1>@PID.3.1</QIP.1>
			   <QIP.2>{{.Identifier}}</QIP.2>
			</QPD.3>
			<QPD.3>
			   <!--PID.3 - Patient Identifier List:-->
			   <QIP.1>@PID.3.4</QIP.1>
			   <QIP.2>{{.Authority}}</QIP.2>
			</QPD.3>
			<QPD.3>
			   <!--PID.3 - Patient Identifier List:-->
			   <QIP.1>@PID.3.5</QIP.1>
			   <QIP.2>{{.AuthorityType}}</QIP.2>
			</QPD.3>
		 </QPD>

		 <RCP>
			<!--Query Priority:-->
			<RCP.1 >I</RCP.1>
			<!--Quantity Limited Request:-->
			<RCP.2 >
			   <CQ.1>50</CQ.1>
			</RCP.2>

		 </RCP>

	  </QBP_Q21>
   </mpi:InvokePatientDemographicsQuery>
</soapenv:Body>
</soapenv:Envelope>
`

// envelope is a struct generated by https://www.onlinetool.io/xmltogo/ from the XML returned from the server.
// However, this doesn't take into account the possibility of repeating fields for certain PID entries.
// See https://hl7-definition.caristix.com/v2/HL7v2.5.1/Segments/PID
// which documents that the following can be repeated: PID3 PID4 PID5 PID6 PID9 PID10 PID11 PID13 PID14 PID21 PID22 PID26 PID32
// Therefore, these have been manually added as []struct rather than struct.
// Also, added PID.29 for date of death
type envelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Text    string   `xml:",chardata"`
	Soap    string   `xml:"soap,attr"`
	Xsi     string   `xml:"xsi,attr"`
	Xsd     string   `xml:"xsd,attr"`
	Body    struct {
		Text                                   string `xml:",chardata"`
		InvokePatientDemographicsQueryResponse struct {
			Text   string `xml:",chardata"`
			Xmlns  string `xml:"xmlns,attr"`
			RSPK21 struct {
				Text  string `xml:",chardata"`
				Xmlns string `xml:"xmlns,attr"`
				MSH   struct {
					Text string `xml:",chardata"`
					MSH1 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
					} `xml:"MSH.1"`
					MSH2 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
					} `xml:"MSH.2"`
					MSH3 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						Table    string `xml:"Table,attr"`
						LongName string `xml:"LongName,attr"`
						HD1      struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"HD.1"`
					} `xml:"MSH.3"`
					MSH4 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						Table    string `xml:"Table,attr"`
						LongName string `xml:"LongName,attr"`
						HD1      struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"HD.1"`
					} `xml:"MSH.4"`
					MSH5 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						Table    string `xml:"Table,attr"`
						LongName string `xml:"LongName,attr"`
						HD1      struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"HD.1"`
					} `xml:"MSH.5"`
					MSH6 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						Table    string `xml:"Table,attr"`
						LongName string `xml:"LongName,attr"`
						HD1      struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"HD.1"`
					} `xml:"MSH.6"`
					MSH7 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
						TS1      struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"TS.1"`
					} `xml:"MSH.7"`
					MSH9 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
						MSG1     struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"MSG.1"`
						MSG2 struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"MSG.2"`
						MSG3 struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"MSG.3"`
					} `xml:"MSH.9"`
					MSH10 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
					} `xml:"MSH.10"`
					MSH11 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
						PT1      struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"PT.1"`
					} `xml:"MSH.11"`
					MSH12 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
						VID1     struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"VID.1"`
					} `xml:"MSH.12"`
					MSH17 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						Table    string `xml:"Table,attr"`
						LongName string `xml:"LongName,attr"`
					} `xml:"MSH.17"`
					MSH19 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
						CE1      struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"CE.1"`
					} `xml:"MSH.19"`
				} `xml:"MSH"`
				MSA struct {
					Text string `xml:",chardata"`
					MSA1 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						Table    string `xml:"Table,attr"`
						LongName string `xml:"LongName,attr"`
					} `xml:"MSA.1"`
					MSA2 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
					} `xml:"MSA.2"`
				} `xml:"MSA"`
				QAK struct {
					Text string `xml:",chardata"`
					QAK1 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
					} `xml:"QAK.1"`
					QAK2 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						Table    string `xml:"Table,attr"`
						LongName string `xml:"LongName,attr"`
					} `xml:"QAK.2"`
				} `xml:"QAK"`
				QPD struct {
					Text string `xml:",chardata"`
					QPD1 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						Table    string `xml:"Table,attr"`
						LongName string `xml:"LongName,attr"`
						CE1      struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"CE.1"`
					} `xml:"QPD.1"`
					QPD2 struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
					} `xml:"QPD.2"`
					QPD3 []struct {
						Text     string `xml:",chardata"`
						Item     string `xml:"Item,attr"`
						Type     string `xml:"Type,attr"`
						LongName string `xml:"LongName,attr"`
						QIP1     struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"QIP.1"`
						QIP2 struct {
							Text     string `xml:",chardata"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"QIP.2"`
					} `xml:"QPD.3"`
				} `xml:"QPD"`
				RSPK21QUERYRESPONSE struct {
					Text string `xml:",chardata"`
					PID  struct {
						Text string `xml:",chardata"`
						PID1 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"PID.1"`
						PID3 []struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							CX1      struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"CX.1"`
							CX4 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								Table    string `xml:"Table,attr"`
								LongName string `xml:"LongName,attr"`
								HD1      struct {
									Text     string `xml:",chardata"`
									Type     string `xml:"Type,attr"`
									Table    string `xml:"Table,attr"`
									LongName string `xml:"LongName,attr"`
								} `xml:"HD.1"`
							} `xml:"CX.4"`
							CX5 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								Table    string `xml:"Table,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"CX.5"`
						} `xml:"PID.3"`
						PID5 []struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							XPN1     struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
								FN1      struct {
									Text     string `xml:",chardata"`
									Type     string `xml:"Type,attr"`
									LongName string `xml:"LongName,attr"`
								} `xml:"FN.1"`
							} `xml:"XPN.1"`
							XPN2 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XPN.2"`
							XPN3 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XPN.3"`
							XPN5 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XPN.5"`
							XPN7 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								Table    string `xml:"Table,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XPN.7"`
						} `xml:"PID.5"`
						PID7 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							TS1      struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"TS.1"`
						} `xml:"PID.7"`
						PID8 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"PID.8"`
						PID9 []struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							XPN7     struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								Table    string `xml:"Table,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XPN.7"`
						} `xml:"PID.9"`
						PID11 []struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							XAD1     struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
								SAD1     struct {
									Text     string `xml:",chardata"`
									Type     string `xml:"Type,attr"`
									LongName string `xml:"LongName,attr"`
								} `xml:"SAD.1"`
							} `xml:"XAD.1"`
							XAD2 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XAD.2"`
							XAD3 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XAD.3"`
							XAD4 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XAD.4"`
							XAD5 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XAD.5"`
							XAD7 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								Table    string `xml:"Table,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XAD.7"`
							XAD13 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								Table    string `xml:"Table,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XAD.13"`
							XAD14 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								Table    string `xml:"Table,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XAD.14"`
						} `xml:"PID.11"`
						PID13 []struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							XTN1     struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XTN.1"`
							XTN2 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								Table    string `xml:"Table,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XTN.2"`
							XTN4 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XTN.4"`
						} `xml:"PID.13"`
						PID14 []struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							XTN1     struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XTN.1"`
							XTN2 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								Table    string `xml:"Table,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XTN.2"`
							XTN4 struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XTN.4"`
						} `xml:"PID.14"`
						PID15 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
							CE1      struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"CE.1"`
						} `xml:"PID.15"`
						PID16 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
							CE1      struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"CE.1"`
						} `xml:"PID.16"`
						PID17 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
							CE1      struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"CE.1"`
						} `xml:"PID.17"`
						PID22 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
							CE1      struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"CE.1"`
						} `xml:"PID.22"`
						PID24 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
						} `xml:"PID.24"`
						PID28 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							Table    string `xml:"Table,attr"`
							LongName string `xml:"LongName,attr"`
							CE1      struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"CE.1"`
						} `xml:"PID.28"`
						PID29 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							TS1      struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"TS.1"`
						} `xml:"PID.29"`
					} `xml:"PID"`
					PD1 struct {
						Text string `xml:",chardata"`
						PD13 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							XON3     struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XON.3"`
						} `xml:"PD1.3"`
						PD14 struct {
							Text     string `xml:",chardata"`
							Item     string `xml:"Item,attr"`
							Type     string `xml:"Type,attr"`
							LongName string `xml:"LongName,attr"`
							XCN1     struct {
								Text     string `xml:",chardata"`
								Type     string `xml:"Type,attr"`
								LongName string `xml:"LongName,attr"`
							} `xml:"XCN.1"`
						} `xml:"PD1.4"`
					} `xml:"PD1"`
				} `xml:"RSP_K21.QUERY_RESPONSE"`
			} `xml:"RSP_K21"`
		} `xml:"InvokePatientDemographicsQueryResponse"`
	} `xml:"Body"`
}
