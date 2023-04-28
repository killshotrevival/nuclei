package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	b64 "encoding/base64"

	"github.com/pkg/errors"

	jsoniter "github.com/json-iterator/go"
	"github.com/logrusorgru/aurora"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/interactsh/pkg/server"
	"github.com/projectdiscovery/nuclei/v2/internal/colorizer"
	"github.com/projectdiscovery/nuclei/v2/pkg/model"
	"github.com/projectdiscovery/nuclei/v2/pkg/model/types/severity"
	"github.com/projectdiscovery/nuclei/v2/pkg/operators"
	"github.com/projectdiscovery/nuclei/v2/pkg/types"
	"github.com/projectdiscovery/nuclei/v2/pkg/utils"
	fileutil "github.com/projectdiscovery/utils/file"
	osutils "github.com/projectdiscovery/utils/os"
)

// Writer is an interface which writes output to somewhere for nuclei events.
type Writer interface {
	// Close closes the output writer interface
	Close()
	// Colorizer returns the colorizer instance for writer
	Colorizer() aurora.Aurora
	// Write writes the event to file and/or screen.
	Write(*ResultEvent) error
	// WriteFailure writes the optional failure event for template to file and/or screen.
	WriteFailure(event InternalEvent) error
	// Request logs a request in the trace log
	Request(templateID, url, requestType string, err error)
	//  WriteStoreDebugData writes the request/response debug data to file
	WriteStoreDebugData(host, templateID, eventType string, data string)
}

// StandardWriter is a writer writing output to file and screen for results.
type StandardWriter struct {
	json                bool
	jsonReqResp         bool
	timestamp           bool
	noMetadata          bool
	matcherStatus       bool
	AstraMeta           AstraMeta
	AstraWebhook        string
	AstraApiServiceName string
	mutex               *sync.Mutex
	aurora              aurora.Aurora
	outputFile          io.WriteCloser
	traceFile           io.WriteCloser
	errorFile           io.WriteCloser
	severityColors      func(severity.Severity) string
	storeResponse       bool
	storeResponseDir    string
}

var decolorizerRegex = regexp.MustCompile(`\x1B\[[0-9;]*[a-zA-Z]`)

// InternalEvent is an internal output generation structure for nuclei.
type InternalEvent map[string]interface{}

// InternalWrappedEvent is a wrapped event with operators result added to it.
type InternalWrappedEvent struct {
	// Mutex is internal field which is implicitly used
	// to synchronize callback(event) and interactsh polling updates
	// Refer protocols/http.Request.ExecuteWithResults for more details
	sync.RWMutex

	InternalEvent   InternalEvent
	Results         []*ResultEvent
	OperatorsResult *operators.Result
	UsesInteractsh  bool
}

func (iwe *InternalWrappedEvent) HasOperatorResult() bool {
	iwe.RLock()
	defer iwe.RUnlock()

	return iwe.OperatorsResult != nil
}

func (iwe *InternalWrappedEvent) HasResults() bool {
	iwe.RLock()
	defer iwe.RUnlock()

	return len(iwe.Results) > 0
}

func (iwe *InternalWrappedEvent) SetOperatorResult(operatorResult *operators.Result) {
	iwe.Lock()
	defer iwe.Unlock()

	iwe.OperatorsResult = operatorResult
}

// ResultEvent is a wrapped result event for a single nuclei output.
type ResultEvent struct {
	// Template is the relative filename for the template
	Template string `json:"template,omitempty"`
	// TemplateURL is the URL of the template for the result inside the nuclei
	// templates repository if it belongs to the repository.
	TemplateURL string `json:"template-url,omitempty"`
	// TemplateID is the ID of the template for the result.
	TemplateID string `json:"template-id"`
	// TemplatePath is the path of template
	TemplatePath string `json:"template-path,omitempty"`
	// Info contains information block of the template for the result.
	Info model.Info `json:"info,inline"`
	// MatcherName is the name of the matcher matched if any.
	MatcherName string `json:"matcher-name,omitempty"`
	// ExtractorName is the name of the extractor matched if any.
	ExtractorName string `json:"extractor-name,omitempty"`
	// Type is the type of the result event.
	Type string `json:"type"`
	// Host is the host input on which match was found.
	Host string `json:"host,omitempty"`
	// Path is the path input on which match was found.
	Path string `json:"path,omitempty"`
	// Matched contains the matched input in its transformed form.
	Matched string `json:"matched-at,omitempty"`
	// ExtractedResults contains the extraction result from the inputs.
	ExtractedResults []string `json:"extracted-results,omitempty"`
	// Request is the optional, dumped request for the match.
	Request string `json:"request,omitempty"`
	// Response is the optional, dumped response for the match.
	Response string `json:"response,omitempty"`
	// Metadata contains any optional metadata for the event
	Metadata map[string]interface{} `json:"meta,omitempty"`
	// IP is the IP address for the found result event.
	IP string `json:"ip,omitempty"`
	// Timestamp is the time the result was found at.
	Timestamp time.Time `json:"timestamp"`
	// Interaction is the full details of interactsh interaction.
	Interaction *server.Interaction `json:"interaction,omitempty"`
	// CURLCommand is an optional curl command to reproduce the request
	// Only applicable if the report is for HTTP.
	CURLCommand string `json:"curl-command,omitempty"`
	// MatcherStatus is the status of the match
	MatcherStatus bool `json:"matcher-status"`
	// Lines is the line count for the specified match
	Lines []int `json:"matched-line"`

	FileToIndexPosition map[string]int `json:"-"`
}

// NewStandardWriter creates a new output writer based on user configurations
func NewStandardWriter(options *types.Options) (*StandardWriter, error) {
	resumeBool := false
	if options.Resume != "" {
		resumeBool = true
	}
	auroraColorizer := aurora.NewAurora(!options.NoColor)

	var outputFile io.WriteCloser
	if options.Output != "" {
		output, err := newFileOutputWriter(options.Output, resumeBool)
		if err != nil {
			return nil, errors.Wrap(err, "could not create output file")
		}
		outputFile = output
	}
	var traceOutput io.WriteCloser
	if options.TraceLogFile != "" {
		output, err := newFileOutputWriter(options.TraceLogFile, resumeBool)
		if err != nil {
			return nil, errors.Wrap(err, "could not create output file")
		}
		traceOutput = output
	}
	var errorOutput io.WriteCloser
	if options.ErrorLogFile != "" {
		output, err := newFileOutputWriter(options.ErrorLogFile, resumeBool)
		if err != nil {
			return nil, errors.Wrap(err, "could not create error file")
		}
		errorOutput = output
	}
	// Try to create output folder if it doesn't exist
	if options.StoreResponse && !fileutil.FolderExists(options.StoreResponseDir) {
		if err := fileutil.CreateFolder(options.StoreResponseDir); err != nil {
			gologger.Fatal().Msgf("Could not create output directory '%s': %s\n", options.StoreResponseDir, err)
		}
	}

	// Load required scan data from environment variable
	tempAstraMeta := AstraMeta{}
	var tempAstraWebhookUrl, tempAstraApiServiceName string

	tempAstraMeta.Event = "alert"
	tempAstraMeta.Hostname = "k8s"

	value, ok := os.LookupEnv("auditId")
	if ok {
		tempAstraMeta.AuditId = value
	} else {
		panic("Audit Id env not present")
	}

	value, ok = os.LookupEnv("jobId")
	if ok {
		tempAstraMeta.JobId = value
	} else {
		panic("Job Id env not present")
	}

	value, ok = os.LookupEnv("scanId")
	if ok {
		tempAstraMeta.ScanId = value
	} else {
		panic("Scan Id env not present")
	}

	value, ok = os.LookupEnv("webhookToken")
	if ok {
		tempAstraMeta.WebhookToken = value
	} else {
		panic("Webhook token env not present")
	}

	value, ok = os.LookupEnv("webhookUrl")
	if ok {
		tempAstraWebhookUrl = value
	} else {
		panic("Webhook url env not present")
	}

	value, ok = os.LookupEnv("DAST_API_SVC_NAME")
	if !ok {
		panic("Support server env not present")
	} else {
		tempAstraApiServiceName = value
	}

	writer := &StandardWriter{
		json:                options.JSONL,
		jsonReqResp:         options.JSONRequests,
		noMetadata:          options.NoMeta,
		matcherStatus:       options.MatcherStatus,
		timestamp:           options.Timestamp,
		aurora:              auroraColorizer,
		mutex:               &sync.Mutex{},
		outputFile:          outputFile,
		traceFile:           traceOutput,
		errorFile:           errorOutput,
		severityColors:      colorizer.New(auroraColorizer),
		storeResponse:       options.StoreResponse,
		storeResponseDir:    options.StoreResponseDir,
		AstraMeta:           tempAstraMeta,
		AstraWebhook:        tempAstraWebhookUrl,
		AstraApiServiceName: tempAstraApiServiceName,
	}

	// Changing state to running
	gologger.Info().Msg("Changing scan state to running")
	writer.sendStatusChangeRequest("RUNNING")
	return writer, nil
}

type sendStatusChangeRequestStruct struct {
	StateChange json.RawMessage `json:"state_change"`
}

// Function for updating status of scan in database
func (w *StandardWriter) sendStatusChangeRequest(action string) {
	gologger.Info().Msgf("Sending status change request with action -> %s\n", action)
	var tempRequest map[string]string

	if action == "RUNNING" {
		tempRequest = map[string]string{"status": action, "pid": "15"}
	} else {
		tempRequest = map[string]string{"status": action}
	}

	tempRequestBody, _ := json.Marshal(tempRequest)
	temp_ := sendStatusChangeRequestStruct{tempRequestBody}

	postBody, _ := json.Marshal(temp_)
	responseBody := bytes.NewBuffer(postBody)
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("http://%s/api/nuclei/%s", w.AstraApiServiceName, w.AstraMeta.ScanId), responseBody)

	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, _ := client.Do(req)

	gologger.Info().Msgf("Status code received for `status change api` -> %s\n", resp.Status)

	// Trigger `scan.complete` event on webhook
	gologger.Info().Msg("Triggering event on webhook url")

	tempAstraRequest := AstraAlertRequest{}
	if action == "RUNNING" {
		w.AstraMeta.Event = "scan.started"
		tempAstraRequest.Context = []byte(`{"reason":"Scan Started successfully"}`)
	} else {
		w.AstraMeta.Event = "scan.complete"
		tempAstraRequest.Context = []byte(`{"reason":"Scan Completed successfully"}`)
	}
	tempAstraRequest.Meta = w.AstraMeta

	postBody_, _ := json.Marshal(tempAstraRequest)
	responseBody_ := bytes.NewBuffer(postBody_)

	resp_, _ := http.Post(w.AstraWebhook, "application/json", responseBody_)

	gologger.Info().Msgf("Request status received -> %s for alert\n", resp_.Status)

}

type AstraMeta struct {
	Event        string `json:"event"`
	AuditId      string `json:"auditId"`
	JobId        string `json:"jobId"`
	ScanId       string `json:"scanId"`
	WebhookToken string `json:"webhookToken"`
	Hostname     string `json:"hostname"`
}

// Request struct that will be used for astra alert's.
type AstraAlertRequest struct {
	Meta    AstraMeta       `json:"meta"`
	Context json.RawMessage `json:"context"`
}

// This function will extract headers and other required data from HTTP raw response string
func extractResponseData(rawResponse string) (string, int, map[string]string) {
	headers := make(map[string]string)
	headerPattern := regexp.MustCompile(`(?m)^([\w-]+):\s*([^\n\r]*)[\n\r]+`)

	// Find all matches of header fields in the raw response string
	matches := headerPattern.FindAllStringSubmatch(rawResponse, -1)

	// Loop through the matches and extract the header name and value
	for _, match := range matches {
		name := strings.ToLower(match[1])
		value := match[2]
		headers[name] = value
	}

	// Extract the status code and HTTP version from the raw response string
	statusPattern := regexp.MustCompile(`^HTTP/(\d+\.\d+)\s+(\d+)\s+.*`)
	statusMatch := statusPattern.FindStringSubmatch(rawResponse)
	httpVersion := ""
	statusCode := 0
	if len(statusMatch) > 2 {
		httpVersion = statusMatch[1]
		statusCode, _ = strconv.Atoi(statusMatch[2])
	}

	return httpVersion, statusCode, headers
}

// Write writes the event to file and/or screen.
func (w *StandardWriter) Write(event *ResultEvent) error {
	// Enrich the result event with extra metadata on the template-path and url.
	if event.TemplatePath != "" {
		event.Template, event.TemplateURL = utils.TemplatePathURL(types.ToString(event.TemplatePath))
	}
	event.Timestamp = time.Now()

	var data []byte
	var err error

	// Extract required data from response string and update response string
	httpVersion, statusCode, headers := extractResponseData(event.Response)
	newResponseString := fmt.Sprintf("HTTP version: %s\nStatus code: %d\n", httpVersion, statusCode)
	for name, value := range headers {
		newResponseString = newResponseString + fmt.Sprintf("%s: %s\n", name, value)
	}
	event.Response = newResponseString

	event.Request = b64.StdEncoding.EncodeToString([]byte(event.Request))
	event.Response = b64.StdEncoding.EncodeToString([]byte(event.Response))

	if w.json {
		data, err = w.formatJSON(event)
	} else {
		data = w.formatScreen(event)
	}
	if err != nil {
		return errors.Wrap(err, "could not format output")
	}
	if len(data) == 0 {
		return nil
	}
	w.mutex.Lock()
	defer w.mutex.Unlock()

	// _, _ = os.Stdout.Write(data)
	// _, _ = os.Stdout.Write([]byte("\n"))

	gologger.Info().Msgf("Raising alert for -> %s\n", event.TemplateURL)

	tempRequest := AstraAlertRequest{}

	w.AstraMeta.Event = "alert"

	tempMeta := w.AstraMeta
	tempRequest.Meta = tempMeta
	tempRequest.Context = data

	postBody, _ := json.Marshal(tempRequest)
	responseBody := bytes.NewBuffer(postBody)

	resp, err := http.Post(w.AstraWebhook, "application/json", responseBody)

	gologger.Info().Msgf("Request status received -> %s for alert\n", resp.Status)

	if w.outputFile != nil {
		if !w.json {
			data = decolorizerRegex.ReplaceAll(data, []byte(""))
		}
		if _, writeErr := w.outputFile.Write(data); writeErr != nil {
			return errors.Wrap(err, "could not write to output")
		}
	}
	return nil
}

// JSONLogRequest is a trace/error log request written to file
type JSONLogRequest struct {
	Template string `json:"template"`
	Input    string `json:"input"`
	Error    string `json:"error"`
	Type     string `json:"type"`
}

// Request writes a log the requests trace log
func (w *StandardWriter) Request(templatePath, input, requestType string, requestErr error) {
	if w.traceFile == nil && w.errorFile == nil {
		return
	}
	request := &JSONLogRequest{
		Template: templatePath,
		Input:    input,
		Type:     requestType,
	}
	if unwrappedErr := utils.UnwrapError(requestErr); unwrappedErr != nil {
		request.Error = unwrappedErr.Error()
	} else {
		request.Error = "none"
	}

	data, err := jsoniter.Marshal(request)
	if err != nil {
		return
	}

	if w.traceFile != nil {
		_, _ = w.traceFile.Write(data)
	}

	if requestErr != nil && w.errorFile != nil {
		_, _ = w.errorFile.Write(data)
	}
}

// Colorizer returns the colorizer instance for writer
func (w *StandardWriter) Colorizer() aurora.Aurora {
	return w.aurora
}

// Close closes the output writing interface
func (w *StandardWriter) Close() {
	gologger.Info().Msg("Execution completed successfully, triggering complete event")

	w.sendStatusChangeRequest("COMPLETE")

	if w.outputFile != nil {
		w.outputFile.Close()
	}
	if w.traceFile != nil {
		w.traceFile.Close()
	}
	if w.errorFile != nil {
		w.errorFile.Close()
	}
}

// WriteFailure writes the failure event for template to file and/or screen.
func (w *StandardWriter) WriteFailure(event InternalEvent) error {
	if !w.matcherStatus {
		return nil
	}
	templatePath, templateURL := utils.TemplatePathURL(types.ToString(event["template-path"]))
	var templateInfo model.Info
	if event["template-info"] != nil {
		templateInfo = event["template-info"].(model.Info)
	}
	data := &ResultEvent{
		Template:      templatePath,
		TemplateURL:   templateURL,
		TemplateID:    types.ToString(event["template-id"]),
		TemplatePath:  types.ToString(event["template-path"]),
		Info:          templateInfo,
		Type:          types.ToString(event["type"]),
		Host:          types.ToString(event["host"]),
		MatcherStatus: false,
		Timestamp:     time.Now(),
	}
	return w.Write(data)
}
func sanitizeFileName(fileName string) string {
	fileName = strings.ReplaceAll(fileName, "http:", "")
	fileName = strings.ReplaceAll(fileName, "https:", "")
	fileName = strings.ReplaceAll(fileName, "/", "_")
	fileName = strings.ReplaceAll(fileName, "\\", "_")
	fileName = strings.ReplaceAll(fileName, "-", "_")
	fileName = strings.ReplaceAll(fileName, ".", "_")
	if osutils.IsWindows() {
		fileName = strings.ReplaceAll(fileName, ":", "_")
	}
	fileName = strings.TrimPrefix(fileName, "__")
	return fileName
}
func (w *StandardWriter) WriteStoreDebugData(host, templateID, eventType string, data string) {
	if w.storeResponse {
		filename := sanitizeFileName(fmt.Sprintf("%s_%s", host, templateID))
		subFolder := filepath.Join(w.storeResponseDir, sanitizeFileName(eventType))
		if !fileutil.FolderExists(subFolder) {
			_ = fileutil.CreateFolder(subFolder)
		}
		filename = filepath.Join(subFolder, fmt.Sprintf("%s.txt", filename))
		f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			fmt.Print(err)
			return
		}
		_, _ = f.WriteString(fmt.Sprintln(data))
		f.Close()
	}

}
