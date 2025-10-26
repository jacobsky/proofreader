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

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
)

func AddRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /proofread", proofread)
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

var systemPrompt = `You are a helpful assistant that assists students in reveiwing and proof reading their prompts.
	First ensure that all responses to the user are in English.
	Second, review the Japanese (日本語）sentences provided by the user.
	Third, breakdown the phrases into individual sentences and then list out the specific mistakes and provide an explanation.
	If the issue is grammatical, please provide the specific reason the grammar is incorrect.
	If the issue is vocabulary, please provide a general definition to the mistaken word as well as the word that the student actually wanted.
	Finally, provide an corrected sentence.
`

type AIAPISignal struct {
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
	Key      string `json:"key"`
	Prompt   string `json:"prompt"`
}

func proofread(w http.ResponseWriter, r *http.Request) {
	var store = &AIAPISignal{}
	if err := datastar.ReadSignals(r, store); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	slog.Info(fmt.Sprintf("Received %v, %v, %v, %v", store.Endpoint, store.Model, store.Key, store.Prompt))

	llm, err := openai.New(
		openai.WithModel(store.Model),
		openai.WithBaseURL(store.Endpoint),
		openai.WithToken(store.Key),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err = sse.PatchElementTempl(OutputSection(WaitingForInput()))

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	err = sse.PatchSignals([]byte(`{ prompt_output: "Waiting for input..."}`))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	output := ""
	resp, err := llm.GenerateContent(
		r.Context(),
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
		},
		llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error {
			content := string(chunk)
			singleline := strings.ReplaceAll(content, "\n", "")
			if singleline == "" {
				return nil
			}
			output = output + singleline
			outputsignal := fmt.Sprintf(`{prompt_output: "%v"}`, output)
			slog.Info("Outputsignal", "sig", singleline)
			return sse.PatchSignals([]byte(outputsignal))
		}))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var buf bytes.Buffer
	if err := md.Convert([]byte(resp.Choices[0].Content), &buf); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		slog.Error("Markdown parsing error", "error", err)
	}
	htmlcontent := unsafeRenderMarkdown(buf.String())
	err = sse.PatchElementTempl(OutputSection(htmlcontent))
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
