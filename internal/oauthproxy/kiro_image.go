package oauthproxy

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"math"
	"strings"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	kiroImageMaxLongSide   = 1568
	kiroImageMaxBase64Size = 400_000
	kiroImageJPEGQuality   = 85
)

func processKiroInlineMedia(conversationState map[string]any) (resized, corrected int) {
	for _, message := range kiroUserMessagesNewestFirst(conversationState) {
		images, _ := message["images"].([]any)
		for _, value := range images {
			wasResized, wasCorrected := processKiroImage(value)
			if wasResized {
				resized++
			}
			if wasCorrected {
				corrected++
			}
		}
	}
	return resized, corrected
}

func processKiroImage(value any) (resized, corrected bool) {
	imageValue, ok := value.(map[string]any)
	if !ok {
		return false, false
	}
	source, ok := imageValue["source"].(map[string]any)
	if !ok {
		return false, false
	}
	encoded, _ := source["bytes"].(string)
	if encoded == "" {
		return false, false
	}
	raw, err := decodeKiroBase64(encoded)
	if err != nil {
		return false, false
	}

	config, detectedFormat, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return false, false
	}
	detectedFormat = normalizeKiroImageFormat(detectedFormat)
	declaredFormat := normalizeKiroImageFormat(metadataString(imageValue, "format"))
	if detectedFormat != "" && detectedFormat != declaredFormat {
		imageValue["format"] = detectedFormat
		corrected = true
	}

	if detectedFormat == "gif" {
		return false, corrected
	}
	if len(encoded) <= kiroImageMaxBase64Size &&
		max(config.Width, config.Height) <= kiroImageMaxLongSide {
		return false, corrected
	}

	decoded, actualFormat, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return false, corrected
	}
	if normalized := normalizeKiroImageFormat(actualFormat); normalized != "" && normalized != detectedFormat {
		imageValue["format"] = normalized
		corrected = true
	}
	output, ok := shrinkKiroImage(decoded)
	if !ok {
		return false, corrected
	}
	source["bytes"] = base64.StdEncoding.EncodeToString(output)
	imageValue["format"] = "jpeg"
	return true, corrected
}

func decodeKiroBase64(value string) ([]byte, error) {
	if comma := strings.IndexByte(value, ','); strings.HasPrefix(value, "data:") && comma >= 0 {
		value = value[comma+1:]
	}
	if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

func normalizeKiroImageFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpg", "jpeg":
		return "jpeg"
	case "png":
		return "png"
	case "gif":
		return "gif"
	case "webp":
		return "webp"
	default:
		return ""
	}
}

func shrinkKiroImage(source image.Image) ([]byte, bool) {
	bounds := source.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	if sourceWidth <= 0 || sourceHeight <= 0 {
		return nil, false
	}
	scale := math.Min(1, float64(kiroImageMaxLongSide)/float64(max(sourceWidth, sourceHeight)))
	width := max(1, int(math.Round(float64(sourceWidth)*scale)))
	height := max(1, int(math.Round(float64(sourceHeight)*scale)))
	quality := kiroImageJPEGQuality

	var best []byte
	for attempt := 0; attempt < 24; attempt++ {
		resized := image.NewRGBA(image.Rect(0, 0, width, height))
		xdraw.Draw(resized, resized.Bounds(), &image.Uniform{C: color.White}, image.Point{}, xdraw.Src)
		xdraw.CatmullRom.Scale(resized, resized.Bounds(), source, bounds, xdraw.Over, nil)

		var output bytes.Buffer
		if err := jpeg.Encode(&output, resized, &jpeg.Options{Quality: quality}); err != nil {
			return nil, false
		}
		best = append(best[:0], output.Bytes()...)
		if base64.StdEncoding.EncodedLen(len(best)) <= kiroImageMaxBase64Size {
			return best, true
		}

		if quality > 55 {
			quality -= 10
			continue
		}
		ratio := math.Sqrt(float64(kiroImageMaxBase64Size)/
			float64(base64.StdEncoding.EncodedLen(len(best)))) * 0.92
		nextWidth := max(1, int(float64(width)*ratio))
		nextHeight := max(1, int(float64(height)*ratio))
		if nextWidth == width && width > 1 {
			nextWidth--
		}
		if nextHeight == height && height > 1 {
			nextHeight--
		}
		width, height = nextWidth, nextHeight
		quality = kiroImageJPEGQuality
	}
	return best, len(best) > 0
}
