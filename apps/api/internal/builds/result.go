package builds

import (
	"encoding/json"
	"errors"
	"io"
)

func readBuildResult(body io.Reader) (string, error) {
	// Bound memory and output processing; BuildKit traces are not user-facing logs.
	limited := &io.LimitedReader{R: body, N: 16*1024*1024 + 1}
	decoder := json.NewDecoder(limited)
	var imageID string
	for {
		var message struct {
			ID          string                   `json:"id"`
			Error       string                   `json:"error"`
			ErrorDetail struct{ Message string } `json:"errorDetail"`
			Aux         json.RawMessage          `json:"aux"`
		}
		err := decoder.Decode(&message)
		if limited.N == 0 {
			return "", errors.New("build response exceeded sixteen MiB")
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", errors.New("invalid or interrupted build response")
		}
		if message.Error != "" || message.ErrorDetail.Message != "" {
			detail := message.ErrorDetail.Message
			if detail == "" {
				detail = message.Error
			}
			if len(detail) > 4096 {
				detail = detail[:4096]
			}
			return "", errors.New(detail)
		}
		if message.ID == "moby.image.id" {
			var result struct{ ID string }
			if json.Unmarshal(message.Aux, &result) != nil || !imageIDPattern.MatchString(result.ID) || imageID != "" && imageID != result.ID {
				return "", errors.New("invalid built image identity")
			}
			imageID = result.ID
		}
	}
	if imageID == "" {
		return "", errors.New("build response did not report an image")
	}
	return imageID, nil
}
