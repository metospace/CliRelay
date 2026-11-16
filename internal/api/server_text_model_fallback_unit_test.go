package api

import "testing"

func TestIsTextGenerationPath(t *testing.T) {
	if !isTextGenerationPath("/v1/chat/completions") || !isTextGenerationPath("/v1/responses/") || !isTextGenerationPath("/v1/messages") {
		t.Fatal("expected common text endpoints to be recognized")
	}
	if isTextGenerationPath("/v1/images/generations") || isTextGenerationPath("/v1/models") {
		t.Fatal("non-text endpoints must not use text-model-fallback")
	}
}

func TestIsTextOnlyModelPayload(t *testing.T) {
	if !isTextOnlyModelPayload([]byte(`{"model":"gpt-6-sol","input":"hi"}`)) {
		t.Fatal("plain text should be eligible")
	}
	if isTextOnlyModelPayload([]byte(`{"model":"gpt-6-sol","modalities":["text","image"]}`)) {
		t.Fatal("multimodal modalities must block fallback")
	}
	if isTextOnlyModelPayload([]byte(`{"model":"gpt-6-sol","tools":[{"type":"image_generation"}]}`)) {
		t.Fatal("image generation tools must block fallback")
	}
	if isTextOnlyModelPayload([]byte(`{"model":"gpt-6-sol","messages":[{"content":[{"type":"input_image"}]}]}`)) {
		t.Fatal("image content must block fallback")
	}
}
