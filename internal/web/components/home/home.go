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
	analyzer      string
	proofreader   string
	suggestor     string
	convertToJSON string
}

func NewHandler(analyzer, proofreader, suggestor, convertToJSON string) *Handler {
	return &Handler{
		analyzer:      analyzer,
		proofreader:   proofreader,
		suggestor:     suggestor,
		convertToJSON: convertToJSON,
	}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("/", h)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:

		store := &AIAPISignal{}

		err := datastar.ReadSignals(r, store)

		sse := datastar.NewSSE(w, r)
		if err != nil {
			handleDatastarError(sse, http.StatusInternalServerError, err)
			return
		}

		slog.Info(fmt.Sprintf("Received %v, %v, %v, %v for view %v", store.Endpoint, store.Model, store.Key, store.Prompt, store.View))
		err = store.validate()
		if err != nil {
			handleDatastarError(sse, http.StatusBadRequest, err)
			return
		}
		switch store.View {
		case "proofread":
			h.assist(sse, store)
		case "suggestion":
			h.suggestion(sse, store)
		}
		handleDatastarError(
			sse,
			400,
			fmt.Errorf("view %v not allowed to call this function", store.View),
		)
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

func handleDatastarError(sse *datastar.ServerSentEventGenerator, status int, err error) {
	// Base case, if no error to handle, skip over it.
	slog.Debug("Error", "status", status, "error-msg", err)
	if err != nil {
		err := sse.ConsoleError(err)
		if err != nil {
			slog.Error("Error", "status", status, "error-msg", err)
		}
	}
}

// Agentic workflow assist function
func (h *Handler) assist(sse *datastar.ServerSentEventGenerator, store *AIAPISignal) {
	// Implement agentic workflow using datastar SSEs
	// Send this to clear the section from the last collection of prompts.
	_ = sse.PatchElementTempl(OutputSection(), datastar.WithSelectorID("output_section"))

	// Step 1: Setup LLM and agents
	llm, err := openai.New(openai.WithModel(store.Model), openai.WithBaseURL(store.Endpoint), openai.WithToken(store.Key))
	if err != nil {
		handleDatastarError(sse, http.StatusBadRequest, err)
		return
	}

	sentences := preprocessInput(store.Prompt)
	responses := []ResponseOutput{}

	store.Status = "Starting"
	_ = sse.MarshalAndPatchSignals(store.Status)

	// Step 1. Analyze each sentence for issues.
	for i, sentence := range sentences {
		slog.Debug("Input", "sentence", sentence)
		llmResponse, err := h.proofread(llm, sse.Context(), sentence)
		if err != nil {
			slog.Error("generation error", "err", err)
			continue
		}

		slog.Debug("Output", "content", llmResponse)

		// LLMs sometimes do not properly parse the json output
		// If this is the case, we need to send an extra request to get it to generate properly.
		var output ResponseOutput
		err = json.Unmarshal([]byte(llmResponse), &output)
		if err != nil {
			output = h.getJSON(llm, sse.Context(), sentence, llmResponse)
			slog.Warn("Initial JSON parsing failed, attempting retry", "err", err, "response", llmResponse)
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
	html, err := h.analyze(llm, sse.Context(), output)
	if err != nil {
		_ = sse.ConsoleError(err)
		return
	}
	_ = sse.PatchElementTempl(Notes(unsafeRenderMarkdown(html)), datastar.WithSelectorID("analysis_notes"))

	_ = sse.PatchElementTempl(HistoryEntry(store.View, store.Prompt, OutputContainer(responses, output, unsafeRenderMarkdown(html))), datastar.WithModeAppend(), datastar.WithSelectorID("history"))
	store.Status = "Complete"
	_ = sse.MarshalAndPatchSignals(store.Status)
}

// Agentic workflow assist function
func (h *Handler) suggestion(sse *datastar.ServerSentEventGenerator, store *AIAPISignal) {
	// Implement agentic workflow using datastar SSEs
	// Send this to clear the section from the last collection of prompts.
	_ = sse.PatchElementTempl(OutputSection(), datastar.WithSelectorID("output_section"))

	// Step 1: Setup LLM and agents
	llm, err := openai.New(openai.WithModel(store.Model), openai.WithBaseURL(store.Endpoint), openai.WithToken(store.Key))
	if err != nil {
		handleDatastarError(sse, http.StatusBadRequest, err)
		return
	}

	store.Status = "Starting"
	_ = sse.MarshalAndPatchSignals(store.Status)

	slog.Debug("Input", "sentence", store.Prompt)
	_ = sse.MarshalAndPatchSignals(store.Status)

	// Step 4. Generate based on the suggestion
	html, err := h.suggestPrompt(llm, sse.Context(), store.Prompt)
	if err != nil {
		_ = sse.ConsoleError(err)
		return
	}
	_ = sse.PatchElementTempl(Notes(unsafeRenderMarkdown(html)), datastar.WithSelectorID("analysis_notes"))

	_ = sse.PatchElementTempl(HistoryEntry(store.View, store.Prompt, OutputContainer([]ResponseOutput{}, "", unsafeRenderMarkdown(html))), datastar.WithModeAppend(), datastar.WithSelectorID("history"))

	store.Status = "Complete"
	_ = sse.MarshalAndPatchSignals(store.Status)
}

func (h *Handler) proofread(llm *openai.LLM, ctx context.Context, sentence string) (string, error) {
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

	response, err := llm.GenerateContent(ctx, messages)
	if err != nil {
		return "", err
	}
	return response.Choices[0].Content, err
}

func (h *Handler) getJSON(llm *openai.LLM, ctx context.Context, sentence, llmResponse string) ResponseOutput {
	// Create retry prompt to reformat as JSON
	retryPrompt := strings.Replace(h.convertToJSON, "{LLM_RESPONSE}", llmResponse, 1)
	retryMessages := []llms.MessageContent{
		{
			Role:  llms.ChatMessageTypeSystem,
			Parts: []llms.ContentPart{llms.TextContent{Text: retryPrompt}},
		},
		{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextContent{Text: sentence}},
		},
	}

	retryResponse, retryErr := llm.GenerateContent(ctx, retryMessages)
	var output ResponseOutput
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
	return output
}

func (h *Handler) analyze(llm *openai.LLM, ctx context.Context, text string) (string, error) {
	messages := []llms.MessageContent{
		{
			Role:  llms.ChatMessageTypeSystem,
			Parts: []llms.ContentPart{llms.TextContent{Text: h.analyzer}},
		},
		{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextContent{Text: text}},
		},
	}

	response, err := llm.GenerateContent(ctx, messages)
	if err != nil {
		slog.Error("generation error", "err", err)
		return "", err
	}

	var buf strings.Builder
	err = md.Convert([]byte(response.Choices[0].Content), &buf)
	if err != nil {
		slog.Error("Markdown parsing error", "err", err)
		return "", err
	}
	html := buf.String()
	return html, nil
}

func (h *Handler) suggestPrompt(llm *openai.LLM, ctx context.Context, text string) (string, error) {
	messages := []llms.MessageContent{
		{
			Role:  llms.ChatMessageTypeSystem,
			Parts: []llms.ContentPart{llms.TextContent{Text: h.suggestor}},
		},
		{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextContent{Text: text}},
		},
	}

	response, err := llm.GenerateContent(ctx, messages)
	if err != nil {
		slog.Error("generation error", "err", err)
		return "", err
	}

	var buf strings.Builder
	err = md.Convert([]byte(response.Choices[0].Content), &buf)
	if err != nil {
		slog.Error("Markdown parsing error", "err", err)
		return "", err
	}
	html := buf.String()
	return html, nil
}

func unsafeRenderMarkdown(html string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) (err error) {
		_, err = io.WriteString(w, html)
		return
	})
}
