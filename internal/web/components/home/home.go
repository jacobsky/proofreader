package home

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	explainer   string
}

func NewHandler(analyzer, proofreader, explainer string) *Handler {
	return &Handler{
		analyzer:    analyzer,
		proofreader: proofreader,
		explainer:   explainer,
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

	// Step 1: Setup LLM and agents
	llm, err := openai.New(openai.WithModel(store.Model), openai.WithBaseURL(store.Endpoint), openai.WithToken(store.Key))
	if err != nil {
		handleDatastarError(sse, w, http.StatusBadRequest, err)
		return
	}

	sentences := preprocessInput(store.Prompt)

	log.Printf("Messages dispatching")
	// responses := []string{}

	store.Status = "Starting"
	_ = sse.MarshalAndPatchSignals(store.Status)

	for i, sentence := range sentences {
		log.Printf("Input: %v", sentence)
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
		log.Printf("Response")
		response, err := llm.GenerateContent(r.Context(), messages)
		if err != nil {
			slog.Error("generation error", "err", err)
			continue
		}

		log.Printf("Output: %v", response.Choices[0].Content)
		var output ResponseOutput
		err = json.Unmarshal([]byte(response.Choices[0].Content), &output)
		if err != nil {
			slog.Error("failed to unmarshal response", "err", err)
			continue
		}
		_ = sse.PatchElementTempl(OutputFragment(output), datastar.WithSelectorID("promptoutput"), datastar.WithModeAppend())
		store.Status = fmt.Sprintf("%v / %v", i, len(sentences))
		_ = sse.MarshalAndPatchSignals(store.Status)
		// responses = append(responses, response.Choices[0].Content)
	}
	store.Status = "Generating Summary"
	_ = sse.MarshalAndPatchSignals(store.Status)
}

func unsafeRenderMarkdown(html string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) (err error) {
		_, err = io.WriteString(w, html)
		return
	})
}
