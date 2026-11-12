package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/httpapi"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

// apiClient talks to the production HTTP surface. In embedded mode it hosts
// the exact handler stack of cmd/voiceservice over httptest; in remote mode
// it targets an already running voiceservice process. Either way every
// create/read/output call crosses the real HTTP boundary.
type apiClient struct {
	baseURL string
	http    *http.Client
	server  *httptest.Server
}

type sourceRequest struct {
	Name      string `json:"name"`
	WAVBase64 string `json:"wav_base64"`
}

type timelineRequest struct {
	Name           string                `json:"name"`
	SampleRate     int                   `json:"sample_rate"`
	OutputChannels int                   `json:"output_channels"`
	TargetPeakDBFS float64               `json:"target_peak_dbfs"`
	Spans          []domain.TimelineSpan `json:"spans"`
}

type renderRequest struct {
	TimelineID string `json:"timeline_id"`
}

type errorEnvelope struct {
	Error domain.ErrorDetail `json:"error"`
}

// newEmbeddedClient starts the real production handler against dataRoot.
func newEmbeddedClient(dataRoot string, maxSourceBytes int64) (*apiClient, error) {
	repo, err := storage.NewRepository(dataRoot)
	if err != nil {
		return nil, err
	}
	serviceLayer := service.New(repo, service.Config{
		MaxSourceBytes:  maxSourceBytes,
		MaxSpans:        service.DefaultConfig().MaxSpans,
		MaxOutputFrames: service.DefaultConfig().MaxOutputFrames,
	})
	handler := httpapi.New(serviceLayer, httpapi.Options{
		MaxSourceRequestBytes: maxSourceBytes * 2,
		MaxJSONBytes:          4 << 20,
	})
	server := httptest.NewServer(handler.Handler())
	return &apiClient{
		baseURL: server.URL,
		http:    &http.Client{Timeout: 60 * time.Second},
		server:  server,
	}, nil
}

// newRemoteClient targets an external voiceservice instance.
func newRemoteClient(baseURL string) *apiClient {
	return &apiClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (client *apiClient) close() {
	if client.server != nil {
		client.server.Close()
	}
}

func (client *apiClient) do(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, data, nil
}

func errorCode(body []byte) string {
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Error.Code
}

// putSource registers a source through PUT /v1/sources/{id}. On a conflict it
// verifies the existing binding carries the same digest; a mismatch is fatal
// because it means the blind ID was rebound to different sound.
func (client *apiClient) putSource(ctx context.Context, id, name string, wav []byte) (domain.SourceAsset, error) {
	status, body, err := client.do(ctx, http.MethodPut, "/v1/sources/"+id, sourceRequest{
		Name:      name,
		WAVBase64: base64.StdEncoding.EncodeToString(wav),
	})
	if err != nil {
		return domain.SourceAsset{}, err
	}
	if status == http.StatusConflict {
		if code := errorCode(body); code != "source_conflict" {
			return domain.SourceAsset{}, fmt.Errorf("source %s unexpected conflict code %s body=%s", id, code, string(body))
		}
		existing, getStatus, getErr := client.getSource(ctx, id)
		if getErr != nil {
			return domain.SourceAsset{}, getErr
		}
		if getStatus != http.StatusOK {
			return domain.SourceAsset{}, fmt.Errorf("source %s conflict but read-back status %d", id, getStatus)
		}
		return existing, nil
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return domain.SourceAsset{}, fmt.Errorf("source %s status %d body=%s", id, status, string(body))
	}
	var asset domain.SourceAsset
	if err := json.Unmarshal(body, &asset); err != nil {
		return domain.SourceAsset{}, err
	}
	return asset, nil
}

func (client *apiClient) getSource(ctx context.Context, id string) (domain.SourceAsset, int, error) {
	status, body, err := client.do(ctx, http.MethodGet, "/v1/sources/"+id, nil)
	if err != nil {
		return domain.SourceAsset{}, 0, err
	}
	var asset domain.SourceAsset
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &asset); err != nil {
			return domain.SourceAsset{}, status, err
		}
	}
	return asset, status, nil
}

func (client *apiClient) putTimeline(
	ctx context.Context,
	id string,
	plan TimelinePlan,
	spans []domain.TimelineSpan,
) (domain.Timeline, error) {
	status, body, err := client.do(ctx, http.MethodPut, "/v1/timelines/"+id, timelineRequest{
		Name:           "blind " + plan.Variant + " intent " + id,
		SampleRate:     plan.SampleRate,
		OutputChannels: plan.OutputChannels,
		TargetPeakDBFS: plan.TargetPeakDBFS,
		Spans:          spans,
	})
	if err != nil {
		return domain.Timeline{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return domain.Timeline{}, fmt.Errorf("timeline %s status %d body=%s", id, status, string(body))
	}
	var timeline domain.Timeline
	if err := json.Unmarshal(body, &timeline); err != nil {
		return domain.Timeline{}, err
	}
	return timeline, nil
}

func (client *apiClient) putRender(ctx context.Context, id, timelineID string) (domain.RenderJob, error) {
	status, body, err := client.do(ctx, http.MethodPut, "/v1/renders/"+id, renderRequest{TimelineID: timelineID})
	if err != nil {
		return domain.RenderJob{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return domain.RenderJob{}, fmt.Errorf("render %s status %d body=%s", id, status, string(body))
	}
	var job domain.RenderJob
	if err := json.Unmarshal(body, &job); err != nil {
		return domain.RenderJob{}, err
	}
	return job, nil
}

func (client *apiClient) getReceipt(ctx context.Context, id string) (domain.RenderReceipt, int, error) {
	status, body, err := client.do(ctx, http.MethodGet, "/v1/renders/"+id+"/receipt", nil)
	if err != nil {
		return domain.RenderReceipt{}, 0, err
	}
	var receipt domain.RenderReceipt
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &receipt); err != nil {
			return domain.RenderReceipt{}, status, err
		}
	}
	return receipt, status, nil
}

func (client *apiClient) getOutput(ctx context.Context, id string) ([]byte, int, error) {
	status, body, err := client.do(ctx, http.MethodGet, "/v1/renders/"+id+"/output", nil)
	if err != nil {
		return nil, 0, err
	}
	return body, status, nil
}
