package home

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/a-h/templ"
	"github.com/starfederation/datastar-go/datastar"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
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
	_ = sse.ExecuteScript(fmt.Sprintf("console.log('Received %v, %v, %v, %v')", store.Endpoint, store.Model, store.Key, store.Prompt))

	_ = sse.PatchSignals([]byte(`{prompt_output: 'This is the prompt output from the server'}`))
	return

	// if err != nil {
	// 	http.Error(w, err.Error(), http.StatusBadRequest)
	// 	return
	// }
	llm, err := openai.New(
		openai.WithModel(store.Model),
		openai.WithBaseURL(store.Endpoint),
		openai.WithToken(store.Key),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	return

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
		})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fmt.Println(resp.Choices[0].Content)
}
