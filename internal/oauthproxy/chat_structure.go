package oauthproxy

import (
	"encoding/json"
	"github.com/tidwall/gjson"
)

// chatContentStructure logs only fixed category names and counts, never text,
// tool arguments, URLs, names or image data from the request.
func chatContentStructure(body []byte) string {
	counts := map[string]int{}
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		counts["messages"]++
		content := message.Get("content")
		switch {
		case content.Type == gjson.String:
			counts["string"]++
		case content.IsArray():
			counts["array"]++
			for _, part := range content.Array() {
				kind := part.Get("type").String()
				switch kind {
				case "text", "image_url", "video_url", "audio_url", "input_audio":
				default:
					kind = "unknown"
				}
				counts["part_"+kind]++
			}
		default:
			counts["null_or_invalid"]++
		}
	}
	raw, _ := json.Marshal(counts)
	return string(raw)
}
