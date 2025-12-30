package home

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/starfederation/datastar-go/datastar"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// Converts the input into setnences
func preprocessInput(input string) []string {
	// Simple sentence splitter for Japanese
	// Split on 。, ！, ？
	sentences := strings.FieldsFunc(input, func(r rune) bool {
		return r == '。' || r == '！' || r == '？' || r == '\n'
	})
	result := make([]string, 0, len(sentences))
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if s != "" {
			result = append(result, s+"。") // add back the period
		}
	}
	return result
}

type Handler struct {
	analyzer    string
	proofreader string
}

func NewHandler(analyzer, proofreader string) *Handler {
	return &Handler{
		analyzer:    analyzer,
		proofreader: proofreader,
	}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /assist", h.assist)
	mux.Handle("/", h)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		templ.Handler(Home()).ServeHTTP(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

type AIAPISignal struct {
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
	Key      string `json:"key"`
	Locked   bool   `json:"locked"`
	Prompt   string `json:"prompt"`
	View     string `json:"view"`
	Status   string `json:"status"`
}

type ResponseOutput struct {
	OriginalSentence  string   `json:"original_sentence"`
	CorrectedSentence string   `json:"corrected_sentence"`
	ErrorDetails      string   `json:"error_details"`
	Reason            string   `json:"reason"`
	MisusedWords      []string `json:"misused_words"`
}

func (signal *AIAPISignal) validate() error {
	errors := make([]string, 0)
	if signal.Model == "" {
		errors = append(errors, "Model is not set. Please set it in the configuration")
	}
	if signal.Endpoint == "" {
		errors = append(errors, "Endpoint is not set. Please set it in the configuration")
	}
	if signal.Key == "" {
		errors = append(errors, "Key is not set. Please set it in the configuration")
	}
	if signal.Prompt == "" {
		errors = append(errors, "Prompt is empty. Please enter a message to submit for inference")
	}
	if len(errors) == 0 {
		return nil
	} else {
		return fmt.Errorf("validation errors\n %v", strings.Join(errors, "\n"))
	}
}

func handleDatastarError(sse *datastar.ServerSentEventGenerator, w http.ResponseWriter, status int, err error) {
	// Base case, if no error to handle, skip over it.
	slog.Debug("Error", "status", status, "error-msg", err)
	if err != nil {
		err := sse.ConsoleError(err)
		if err != nil {
			slog.Error("Error", "status", status, "error-msg", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// Agentic workflow assist function
func (h *Handler) assist(w http.ResponseWriter, r *http.Request) {
	// Implement agentic workflow using datastar SSEs
	store := &AIAPISignal{}

	err := datastar.ReadSignals(r, store)

	sse := datastar.NewSSE(w, r)
	if err != nil {
		handleDatastarError(sse, w, http.StatusInternalServerError, err)
		return
	}

	slog.Info(fmt.Sprintf("Received %v, %v, %v, %v for view %v", store.Endpoint, store.Model, store.Key, store.Prompt, store.View))
	err = store.validate()
	if err != nil {
		handleDatastarError(sse, w, http.StatusBadRequest, err)
		return
	}
	// Send this to clear the section from the last collection of prompts.
	_ = sse.PatchElementTempl(OutputSection(), datastar.WithSelectorID("output_section"))

	// Step 1: Setup LLM and agents
	llm, err := openai.New(openai.WithModel(store.Model), openai.WithBaseURL(store.Endpoint), openai.WithToken(store.Key))
	if err != nil {
		handleDatastarError(sse, w, http.StatusBadRequest, err)
		return
	}

	sentences := preprocessInput(store.Prompt)
	responses := []ResponseOutput{}

	store.Status = "Starting"
	_ = sse.MarshalAndPatchSignals(store.Status)

	// Step 1. Analyze each sentence for issues.
	for i, sentence := range sentences {
		slog.Debug("Input", "sentence", sentence)
		messages := []llms.MessageContent{
			{
				Role:  llms.ChatMessageTypeSystem,
				Parts: []llms.ContentPart{llms.TextContent{Text: h.proofreader}},
			},
			{
				Role:  llms.ChatMessageTypeHuman,
				Parts: []llms.ContentPart{llms.TextContent{Text: sentence}},
			},
		}
		response, err := llm.GenerateContent(r.Context(), messages)
		if err != nil {
			slog.Error("generation error", "err", err)
			continue
		}

		slog.Debug("Output", "content", response.Choices[0].Content)

		var output ResponseOutput
		llmResponse := response.Choices[0].Content

		// LLMs sometimes do not properly parse the json output
		// If this is the case, we need to send an extra request to get it to generate properly.
		err = json.Unmarshal([]byte(llmResponse), &output)
		if err != nil {
			slog.Warn("Initial JSON parsing failed, attempting retry", "err", err, "response", llmResponse)

			// Create retry prompt to reformat as JSON
			retryMessages := []llms.MessageContent{
				{
					Role: llms.ChatMessageTypeSystem,
					Parts: []llms.ContentPart{llms.TextContent{Text: `You must respond with VALID JSON only. Your previous response was not valid JSON.
						Reformat this response as proper JSON:
						` + llmResponse + `
						Return ONLY a JSON object with this exact structure:
						{
						"original_sentence": "string",
						"corrected_sentence": "string",
						"error_details": "string",
						"reason": "string",
						"misused_words": []
						}`}},
				},
				{
					Role:  llms.ChatMessageTypeHuman,
					Parts: []llms.ContentPart{llms.TextContent{Text: sentence}},
				},
			}

			retryResponse, retryErr := llm.GenerateContent(r.Context(), retryMessages)
			if retryErr != nil {
				slog.Error("retry generation failed", "err", retryErr)
				// Use fallback response instead of skipping
				output = ResponseOutput{
					OriginalSentence:  sentence,
					CorrectedSentence: sentence,
					ErrorDetails:      "Unable to process - AI returned invalid format",
					Reason:            "",
					MisusedWords:      []string{},
				}
			} else {
				llmRetryResponse := retryResponse.Choices[0].Content
				slog.Debug("Retry output", "content", llmRetryResponse)

				retryErr = json.Unmarshal([]byte(llmRetryResponse), &output)
				if retryErr != nil {
					slog.Error("retry JSON parsing also failed", "err", retryErr, "response", llmRetryResponse)
					// Use fallback response
					output = ResponseOutput{
						OriginalSentence:  sentence,
						CorrectedSentence: sentence,
						ErrorDetails:      "Unable to process - AI returned invalid format after retry",
						Reason:            "",
						MisusedWords:      []string{},
					}
				}
			}
		}
		_ = sse.PatchElementTempl(OutputFragment(output), datastar.WithSelectorID("promptoutput"), datastar.WithModeAppend())

		store.Status = fmt.Sprintf("%v / %v", i, len(sentences))
		_ = sse.MarshalAndPatchSignals(store.Status)

		responses = append(responses, output)
	}

	correctedString := strings.Builder{}
	for _, r := range responses {
		correctedString.WriteString(r.CorrectedSentence)
		correctedString.WriteString(" ")
	}

	output := correctedString.String()
	_ = sse.PatchElementTempl(FinalOutput(output), datastar.WithSelectorID("final_output"))

	store.Status = "Generating Analysis"
	_ = sse.MarshalAndPatchSignals(store.Status)

	// Step 4. Generate a general analysis of the corrected input text
	messages := []llms.MessageContent{
		{
			Role:  llms.ChatMessageTypeSystem,
			Parts: []llms.ContentPart{llms.TextContent{Text: h.analyzer}},
		},
		{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextContent{Text: output}},
		},
	}
	response, err := llm.GenerateContent(r.Context(), messages)
	if err != nil {
		slog.Error("generation error", "err", err)
		return
	}

	var buf strings.Builder
	md.Convert([]byte(response.Choices[0].Content), &buf)
	html := buf.String()

	_ = sse.PatchElementTempl(Notes(unsafeRenderMarkdown(html)), datastar.WithSelectorID("analysis_notes"))

	store.Status = "Complete"
	_ = sse.MarshalAndPatchSignals(store.Status)
}

func unsafeRenderMarkdown(html string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) (err error) {
		_, err = io.WriteString(w, html)
		return
	})
}
