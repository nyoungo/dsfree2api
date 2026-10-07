package api

import (
	"context"

	"github.com/nyoungo/dsfree2api/internal/openai"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

// continuationFor builds the prompt function that asks the model to resume a
// tool-call reply the site's per-reply output cap cut off. It returns nil
// when no tools were offered, which disables continuation entirely.
func continuationFor(tools []openai.ToolDef, messages []openai.ChatMessage, rounds int) openai.ContinuationFunc {
	if len(tools) == 0 || len(messages) == 0 {
		return nil
	}
	base := append([]openai.ChatMessage{}, messages...)
	return func(partial string, round int) string {
		history := append(append([]openai.ChatMessage{}, base...),
			openai.ChatMessage{Role: "assistant", Content: openai.MessageContent{Text: openai.ContinuationContext(partial, 400, 2000)}},
			openai.ChatMessage{Role: "user", Content: openai.MessageContent{Text: openai.ContinueInstruction(partial, round, rounds)}},
		)
		return openai.BuildPrompt(history, openai.PromptOptions{})
	}
}

// continueRounds reads the configured number of continuation turns.
func (s *Server) continueRounds() int {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	return s.cfg.Upstream.ContinueRounds
}

// chatWithContinue runs one upstream conversation and transparently stitches
// truncated tool-call JSON replies across extra continuation turns. emit
// receives the reply text in order; firstDelta is stamped on the first delta
// of any turn.
func (s *Server) chatWithContinue(
	ctx context.Context,
	modelID, prompt string,
	cont openai.ContinuationFunc,
	info *upstream.ServeInfo,
	onFirstDelta func(),
	emit func(text string) error,
) error {
	return openai.RunContinued(ctx, prompt, cont, s.continueRounds(),
		func(ctx context.Context, p string, emitText func(string) error) error {
			return s.up.Chat(ctx, modelID, p, info, func(ev upstream.Event) error {
				if ev.Kind == upstream.KindDelta && ev.Value != "" {
					if onFirstDelta != nil {
						onFirstDelta()
					}
					return emitText(ev.Value)
				}
				return nil
			})
		},
		emit,
		func(msg string, args ...any) { s.log.Info(msg, args...) },
		func(msg string, args ...any) { s.log.Warn(msg, args...) },
	)
}
