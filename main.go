package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-tfe"
)

type PrePlanPayload struct {
	PayloadVersion                  int    `json:"payload_version"`
	AccessToken                     string `json:"access_token"`
	Stage                           string `json:"stage"`
	IsSpeculative                   bool   `json:"is_speculative"`
	TaskResultID                    string `json:"task_result_id"`
	TaskResultEnforcementLevel      string `json:"task_result_enforcement_level"`
	TaskResultCallbackURL           string `json:"task_result_callback_url"`
	RunAppURL                       string `json:"run_app_url"`
	RunID                           string `json:"run_id"`
	RunMessage                      string `json:"run_message"`
	RunCreatedAt                    string `json:"run_created_at"`
	RunCreatedBy                    string `json:"run_created_by"`
	WorkspaceID                     string `json:"workspace_id"`
	WorkspaceName                   string `json:"workspace_name"`
	WorkspaceAppURL                 string `json:"workspace_app_url"`
	OrganizationName                string `json:"organization_name"`
	VCSRepoURL                      string `json:"vcs_repo_url"`
	VCSBranch                       string `json:"vcs_branch"`
	VCSPullRequestURL               string `json:"vcs_pull_request_url"`
	VCSCommitURL                    string `json:"vcs_commit_url"`
	ConfigurationVersionID          string `json:"configuration_version_id"`
	ConfigurationVersionDownloadURL string `json:"configuration_version_download_url"`
	WorkspaceWorkingDirectory       string `json:"workspace_working_directory"`
}

type Result struct {
	Data ResultData `json:"data"`
}
type ResultData struct {
	Type       string           `json:"type"`
	Attributes ResultAttributes `json:"attributes"`
}

type ResultAttributes struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Queue to store the jobs (JSON payloads)
var jobQueue = make(chan PrePlanPayload, 100)

var restrictedCredentialKeys = map[string]struct{}{
	"AWS_ACCESS_KEY_ID":      struct{}{},
	"AWS_SECRET_ACCESS_KEY":  struct{}{},
	"AWS_SESSION_EXPIRATION": struct{}{},
	"AWS_SESSION_TOKEN":      struct{}{},
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// Check if the request method is POST
	if r.Method != http.MethodPost || r.UserAgent() != "TFC/1.0 (+https://app.terraform.io; TFC)" {
		http.Error(w, "You aren't a TFC Run Task, go away", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Parse the JSON payload
	var payload PrePlanPayload
	err = json.Unmarshal(body, &payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Println(err.Error())
		return
	}

	// Add the job to the queue
	jobQueue <- payload

	// Respond with an HTTP 200 OK status
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("200 OK"))
}

func main() {
	tfeClient, err := tfe.NewClient(nil) // Use defaults
	if err != nil {
		log.Fatal(err)
	}

	// Start the job processor in a separate goroutine
	go processJobs(tfeClient)

	// Define the HTTP handler function
	http.HandleFunc("/", handleRequest)

	// Start the server on port 80
	log.Println("Server listening on port 80...")
	log.Fatal(http.ListenAndServe(":80", nil))
}

func sendPatchRequest(url string, payload []byte, authToken string) error {
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewBuffer(payload))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("Authorization", "Bearer "+authToken)

	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil || resp == nil {
		return err
	}

	defer resp.Body.Close()
	// Read the response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// Convert the response body to a string
	respBodyStr := string(respBody)

	// Process the response as needed
	if resp.StatusCode != http.StatusOK {
		log.Println(respBodyStr)
		return err
	}

	return nil
}

func processJobs(tfeClient *tfe.Client) {
	for payload := range jobQueue {
		log.Printf("Processing job for run: %+v\n", payload.RunID)

		workspaceVars, err := tfeClient.Variables.List(context.Background(), payload.WorkspaceID, nil)
		if err != nil {
			log.Println(err.Error())
		}

		var foundKeys []string

		for _, variable := range workspaceVars.Items {
			if _, ok := restrictedCredentialKeys[variable.Key]; ok {
				foundKeys = append(foundKeys, variable.Key)
			}
		}

		if len(foundKeys) != 0 {
			log.Println("Looks like someone accidentally set their own AWS creds! Let's steer them in the right direction...")
			message := fmt.Sprintf(`
This workspace appears to have AWS credential variables set on it. AWS credentials are managed on your behalf by the Platform Engineering team. These must be removed immediately to ensure compliance: %s. Go to the "Variables" page in the left side nav and remove these variables, then start another run. If you have any questions, feel free to reach out to platform@mycoolcompany.com. Thanks! `, strings.Join(foundKeys, ", "))
			result := createFailedResult(message)
			jsonData, err := json.Marshal(result)
			if err != nil {
				log.Println(err.Error())
			}

			err = sendPatchRequest(payload.TaskResultCallbackURL, jsonData, payload.AccessToken)
			if err != nil {
				log.Println(err.Error())
			}

		} else {
			result := createPassedResult("No erroenous credentials set on this workspace. Good job! --Platform Engineering Team")
			jsonData, err := json.Marshal(result)
			if err != nil {
				log.Println(err.Error())
			}

			err = sendPatchRequest(payload.TaskResultCallbackURL, jsonData, payload.AccessToken)
			if err != nil {
				log.Println(err.Error())
			}
		}

		log.Println("Job complete for run: %s", payload.RunID)

		// Sleep for some time before checking for the next job
		time.Sleep(1 * time.Second)
	}
}

func createPassedResult(message string) Result {
	return Result{
		Data: ResultData{
			Type: "task-results",
			Attributes: ResultAttributes{
				Status:  "passed",
				Message: message,
				URL:     "",
			},
		},
	}
}
func createFailedResult(message string) Result {
	return Result{
		Data: ResultData{
			Type: "task-results",
			Attributes: ResultAttributes{
				Status:  "failed",
				Message: message,
				URL:     "",
			},
		},
	}
}
