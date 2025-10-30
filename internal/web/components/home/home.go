package home

import (
	"bytes"
	"context"
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

var proofreaderPrompt = `You are a helpful assistant that assists students in reveiwing and proof reading their prompts.
	First ensure that all responses to the user are in English.
	Second, review the Japanese (日本語）sentences provided by the user.
	Third, breakdown the phrases into individual sentences and then list out the specific mistakes and provide an explanation.
	If the issue is grammatical, please provide the specific reason the grammar is incorrect.
	If the issue is vocabulary, please provide a general definition to the mistaken word as well as the word that the student actually wanted.
	Finally, provide an corrected sentence.
`

var suggestorPrompt = `You are a helpful assistant that assists language students with translating English sentences to Japanese.
	First ensure that all responses to the user is in English
	Second, repond to them by reading their entire question, breaking it down into separate sentences, and then translating them one by one.
	Third, explain the reason behind each word choice and give a brief summary of the sentence structure.
	For the output, please ensure that the suggested translations is first, then provide the explanation below.
	`

var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

func AddRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /assist", assist)
	mux.Handle("/", newHandler())
}

type handler struct{}

func newHandler() http.Handler {
	return &handler{}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		err := sse.PatchElementTempl(OutputSection(ErrorMessage(status, err)))
		if err != nil {
			slog.Error("Error", "status", status, "error-msg", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func assist(w http.ResponseWriter, r *http.Request) {
	// Implement a URI parameter to differentiate the prompt that will be used
	var store = &AIAPISignal{}

	err := datastar.ReadSignals(r, store) // sse.PatchSignals([]byte(`{fetching: true}`))

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

	llm, err := openai.New(
		openai.WithModel(store.Model),
		openai.WithBaseURL(store.Endpoint),
		openai.WithToken(store.Key),
	)
	if err != nil {
		handleDatastarError(sse, w, http.StatusBadRequest, err)
		return
	}
	err = sse.PatchElementTempl(OutputSection(WaitingForInput()))

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = sse.PatchSignals([]byte(`{ prompt_output: "Waiting for input..."}`))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var systemPrompt string
	switch store.View {
	case "proofread":
		systemPrompt = proofreaderPrompt
	case "suggestion":
		systemPrompt = suggestorPrompt
	default:
		handleDatastarError(sse, w, http.StatusInternalServerError, fmt.Errorf("view [%v] is not valid please fix the frontend template", store.View))
	}
	output := ""
	messages :=
		[]llms.MessageContent{
			{
				Role: llms.ChatMessageTypeSystem,
				Parts: []llms.ContentPart{
					llms.TextContent{
						Text: systemPrompt,
					},
				},
			},
			{
				Role: llms.ChatMessageTypeHuman,
				Parts: []llms.ContentPart{
					llms.TextContent{
						Text: store.Prompt,
					},
				},
			},
		}
	resp, err := llm.GenerateContent(
		r.Context(),
		messages,
		llms.WithStreamThinking(true),
		llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error {
			content := string(chunk)
			singleline := strings.ReplaceAll(content, "\n", "")
			if singleline == "" {
				return nil
			}
			output = output + singleline
			outputsignal := fmt.Sprintf(`{prompt_output: "%v"}`, output)
			return sse.PatchSignals([]byte(outputsignal))
		}))
	if err != nil {
		handleDatastarError(sse, w, http.StatusInternalServerError, err)
		return
	}

	var buf bytes.Buffer
	if err := md.Convert([]byte(resp.Choices[0].Content), &buf); err != nil {
		handleDatastarError(sse, w, http.StatusInternalServerError, fmt.Errorf("an internal error has occurred"))
	}
	htmlcontent := unsafeRenderMarkdown(buf.String())
	// sse.PatchSignals([]byte(`{fetching: false}`))
	err = sse.PatchElementTempl(OutputSection(htmlcontent))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	err = sse.PatchElementTempl(HistoryEntry(store.View, store.Prompt, htmlcontent), datastar.WithSelectorID("history"), datastar.WithModePrepend())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func unsafeRenderMarkdown(html string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) (err error) {
		_, err = io.WriteString(w, html)
		return
	})
}
