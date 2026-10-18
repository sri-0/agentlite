package providers

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"

	"agentlite/config"
)

// TestEmbedParams: what goes on the wire. Texts are an OpenAI input list;
// a file is one chat message in input, with OpenAI's part types where there
// are some; dimensions only when configured.
func TestEmbedParams(t *testing.T) {
	media := func(mediaType string) openai.EmbeddingNewParamsInputUnion {
		message := openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{mediaPart([]byte("x"), mediaType)})
		return param.Override[openai.EmbeddingNewParamsInputUnion]([]openai.ChatCompletionMessageParamUnion{message})
	}
	for _, tc := range []struct {
		name  string
		dims  int
		input openai.EmbeddingNewParamsInputUnion
		want  string
	}{
		{
			name: "texts", dims: 768, input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: []string{"a", "b"}},
			want: `{"input":["a","b"],"model":"m","dimensions":768,"encoding_format":"float"}`,
		},
		{
			name: "no dimensions", input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: []string{"a"}},
			want: `{"input":["a"],"model":"m","encoding_format":"float"}`,
		},
		{
			name: "image", input: media("image/png"),
			want: `{"input":[{"content":[{"image_url":{"url":"data:image/png;base64,eA=="},"type":"image_url"}],"role":"user"}],"model":"m","encoding_format":"float"}`,
		},
		{
			name: "audio", input: media("audio/mpeg"),
			want: `{"input":[{"content":[{"input_audio":{"data":"eA==","format":"mp3"},"type":"input_audio"}],"role":"user"}],"model":"m","encoding_format":"float"}`,
		},
		{
			name: "video", input: media("video/mp4"),
			want: `{"input":[{"content":[{"type":"video_url","video_url":{"url":"data:video/mp4;base64,eA=="}}],"role":"user"}],"model":"m","encoding_format":"float"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Embedder{cfg: config.EmbeddingConfig{Dimensions: tc.dims}}
			got, err := json.Marshal(e.params("m", tc.input))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}
