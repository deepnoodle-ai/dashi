package gemini

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/mp4"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

// vertexVideoLocation is the only region Google documents for Veo on Vertex.
const vertexVideoLocation = "us-central1"

// maxVertexFrameBytes is Vertex's documented limit for an inline first frame.
const maxVertexFrameBytes = 20 << 20

// maxVertexOperationBytes bounds a done operation, which holds the video as
// base64: four bytes for every three, plus room for the JSON around it.
const maxVertexOperationBytes = maxVideoBytes*4/3 + 1<<20

// VertexVideoConfig describes one Vertex AI project and location for Veo
// jobs. It resolves Project and credentials as a Vertex Config does: the zero
// value reads GOOGLE_CLOUD_PROJECT and uses Application Default Credentials.
type VertexVideoConfig struct {
	// Provider defaults to "google-vertex". A custom Endpoint requires it.
	Provider string
	// Project defaults to GOOGLE_CLOUD_PROJECT.
	Project string
	// Location defaults to GOOGLE_CLOUD_LOCATION, then GOOGLE_CLOUD_REGION,
	// then "us-central1", the only region Google documents for Veo. A value
	// that resolves to "global" becomes "us-central1", so a location shared
	// with model calls still works; any other value is used as given.
	// Veo is documented only in us-central1; set Location if your
	// environment names another region.
	Location string
	// CredentialScope names the credential principal in request origins
	// without storing the credential. It defaults to "project:" + Project.
	CredentialScope string
	// Endpoint defaults to the documented predictLongRunning URL for Project
	// and Location. It must contain {model} and end in that project and
	// location's publishers/google/models/{model}:predictLongRunning path.
	Endpoint string
	// Client supplies transport and credentials. When it is set, Dashi adds
	// no token. When it is nil, Dashi sends an ADC token with the
	// cloud-platform scope, and only to an https Vertex AI host:
	// aiplatform.googleapis.com, {location}-aiplatform.googleapis.com, or
	// aiplatform.{us,eu}.rep.googleapis.com.
	Client *http.Client
	// VideoModels adds to and replaces the built-in model info. Evidence must
	// name the model's resolved endpoint and VideoAPI.
	VideoModels      map[string]media.ModelInfo
	CapabilityPolicy model.CapabilityPolicy
}

// VertexVideoJobClient runs Veo jobs on Vertex AI. Vertex returns the video
// inline in the done operation, so the succeeded poll carries the bytes and
// FetchVideo only checks and measures them. It never retries a submission.
type VertexVideoJobClient struct {
	config VertexVideoConfig
	models map[string]media.ModelInfo
	// adc is true when Dashi attaches its own ADC token.
	adc bool
	// client is the HTTP client for sends; nil means doGeminiMedia's default.
	client *http.Client
	detect func() (*auth.Credentials, error)

	mu          sync.Mutex
	credentials *auth.Credentials
}

var _ media.VideoJobClient = (*VertexVideoJobClient)(nil)

// NewVertexVideoJobs resolves the project, location, and endpoint and
// snapshots model info. It does not look for credentials; the first send
// does, so a missing ADC fails SubmitVideo before anything is sent.
func NewVertexVideoJobs(config VertexVideoConfig) (*VertexVideoJobClient, error) {
	if config.Project == "" {
		config.Project = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	if config.Project == "" {
		return nil, errors.New("gemini: Vertex video requires a project: set Project or GOOGLE_CLOUD_PROJECT")
	}
	config.Location = cmp.Or(
		config.Location,
		os.Getenv("GOOGLE_CLOUD_LOCATION"),
		os.Getenv("GOOGLE_CLOUD_REGION"),
		vertexVideoLocation,
	)
	if config.Location == "global" {
		config.Location = vertexVideoLocation
	}
	if config.CredentialScope == "" {
		config.CredentialScope = "project:" + config.Project
	}
	scopePath := fmt.Sprintf(
		"/projects/%s/locations/%s/publishers/google/models/{model}:predictLongRunning",
		url.PathEscape(config.Project),
		url.PathEscape(config.Location),
	)
	defaultEndpoint := "https://" + vertexHost(config.Location) + "/v1" + scopePath
	if config.Endpoint == "" {
		config.Endpoint = defaultEndpoint
	}
	if config.Provider == "" {
		if config.Endpoint != defaultEndpoint {
			return nil, errors.New("gemini: custom Vertex video endpoint requires provider identity")
		}
		config.Provider = "google-vertex"
	}
	if err := validateMediaEndpoint(
		config.Endpoint,
		":predictLongRunning",
	); err != nil {
		return nil, err
	}
	u, _ := url.Parse(config.Endpoint)
	if !strings.HasSuffix(u.Path, scopePath) {
		return nil, errors.New("gemini: Vertex video endpoint does not match project and location")
	}
	if config.Client == nil && (u.Scheme != "https" || !isVertexHost(u.Host)) {
		return nil, errors.New("gemini: Vertex video endpoint outside an https Vertex AI host requires a Client")
	}
	if !config.CapabilityPolicy.Valid() {
		return nil, errors.New("gemini: invalid unknown media capability policy")
	}
	p := &VertexVideoJobClient{
		config: config,
		models: map[string]media.ModelInfo{},
		adc:    config.Client == nil,
		detect: vertexCredentials,
	}
	if config.Client != nil {
		client := *config.Client
		p.client = &client
		p.config.Client = &client
	}
	for _, name := range []string{
		"veo-3.1-generate-001",
		"veo-3.1-fast-generate-001",
		"veo-3.1-lite-generate-001",
	} {
		p.models[name] = p.builtInModel(name)
	}
	for name, info := range config.VideoModels {
		if err := validateMediaModelInfo(
			name,
			info,
			VideoAPI,
			p.videoURL(name),
			false,
		); err != nil {
			return nil, err
		}
		p.models[name] = info.Clone()
	}
	p.config.VideoModels = nil
	return p, nil
}

// vertexModelDate is when Dashi read Google's Veo model pages.
var vertexModelDate = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

// builtInModel returns Dashi's model info for a Vertex Veo model. Fast lists
// 4k only on copies of the price list, so its evidence is an inference.
func (p *VertexVideoJobClient) builtInModel(name string) media.ModelInfo {
	video := &media.VideoCapabilities{
		Generate:        model.SupportSupported,
		Edit:            model.SupportUnsupported,
		Extend:          model.SupportUnsupported,
		ReferenceFrames: model.SupportSupported,
		Resolutions:     []string{"720p", "1080p", "4k"},
		Durations:       []time.Duration{4 * time.Second, 6 * time.Second, 8 * time.Second},
		AspectRatios:    []string{"16:9", "9:16"},
		OutputMIMETypes: []string{"video/mp4"},
	}
	evidence := model.ModelEvidence{
		Model:       name,
		API:         VideoAPI,
		Endpoint:    p.videoURL(name),
		Owner:       "Dashi",
		Source:      "https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/veo/3-1-generate",
		Method:      model.EvidenceDocumentation,
		QualifiedAt: vertexModelDate,
	}
	switch name {
	case "veo-3.1-fast-generate-001":
		evidence.Method = model.EvidenceInference
		evidence.Source += " (4k from price-list copies, untested)"
	case "veo-3.1-lite-generate-001":
		// Lite's page contradicts itself on image input.
		video.Resolutions = []string{"720p", "1080p"}
		video.ReferenceFrames = model.SupportUnknown
	}
	return media.ModelInfo{Video: video, Evidence: evidence}
}

// Provider returns the configured destination identity.
func (p *VertexVideoJobClient) Provider() string {
	return p.config.Provider
}

// VideoModelInfo returns an independent snapshot of video evidence.
func (p *VertexVideoJobClient) VideoModelInfo(name string) (media.ModelInfo, bool) {
	info, ok := p.models[name]
	return info.Clone(), ok
}

// PrepareVideo encodes a Veo text or first-frame request without sending it.
// Audio is on unless Settings.DisableAudio is set, and the request always
// says which, because Google has changed its default before. Dashi does not
// apply the Gemini API's rule that 1080p and 4k need 8 s; Vertex decides.
func (p *VertexVideoJobClient) PrepareVideo(
	ctx context.Context,
	in media.VideoInput,
) (media.VideoRequest, error) {
	if err := ctx.Err(); err != nil {
		return media.VideoRequest{}, err
	}
	if err := in.Validate(); err != nil {
		return media.VideoRequest{}, p.failure(media.ErrorInvalidRequest, "%w", err)
	}
	if in.Operation != "" && in.Operation != media.VideoGenerate {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Vertex Veo %s is not supported",
			in.Operation,
		)
	}
	if in.Settings.Size.Width != 0 {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Vertex Veo exact size %dx%d; use resolution and aspect ratio",
			in.Settings.Size.Width,
			in.Settings.Size.Height,
		)
	}
	if len(in.ReferenceFrames) > 1 {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Vertex Veo takes one first frame, not %d",
			len(in.ReferenceFrames),
		)
	}
	if ratio := in.Settings.AspectRatio; ratio != "" &&
		ratio != "16:9" && ratio != "9:16" {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Vertex Veo aspect ratio %q",
			ratio,
		)
	}
	if d := in.Settings.Duration; d != 0 &&
		d != 4*time.Second && d != 6*time.Second && d != 8*time.Second {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Vertex Veo duration %s",
			d,
		)
	}
	if resolution := in.Settings.Resolution; resolution != "" &&
		resolution != "720p" && resolution != "1080p" && resolution != "4k" {
		return media.VideoRequest{}, p.failure(
			media.ErrorUnsupported,
			"Vertex Veo resolution %q",
			resolution,
		)
	}
	var info *media.ModelInfo
	capabilities := media.VideoCapabilities{}
	if known, ok := p.VideoModelInfo(in.Model); ok {
		info, capabilities = &known, *known.Video
	}
	unverified, err := capabilities.Check(in, p.config.CapabilityPolicy)
	if err != nil {
		return media.VideoRequest{}, p.failure(media.ErrorUnsupported, "%w", err)
	}
	var prompt []string
	for _, b := range in.Prompt {
		if b.Kind != content.KindText || len(b.Text.Citations) > 0 {
			return media.VideoRequest{}, p.failure(
				media.ErrorUnsupported,
				"Vertex Veo prompt requires uncited text",
			)
		}
		prompt = append(prompt, b.Text.Text)
	}
	if len(in.ReferenceFrames) == 0 &&
		strings.TrimSpace(strings.Join(prompt, "")) == "" {
		return media.VideoRequest{}, p.failure(
			media.ErrorInvalidRequest,
			"Vertex Veo requires a nonempty text prompt or first frame",
		)
	}
	instance := map[string]any{}
	if len(prompt) > 0 {
		instance["prompt"] = strings.Join(prompt, "\n\n")
	}
	if len(in.ReferenceFrames) == 1 {
		source := in.ReferenceFrames[0].Image.Source.WithDetectedMIMEType("")
		if source.Data == nil ||
			(source.MIMEType != "image/png" && source.MIMEType != "image/jpeg") {
			return media.VideoRequest{}, p.failure(
				media.ErrorUnsupported,
				"Vertex Veo first frame requires inline PNG or JPEG bytes",
			)
		}
		if len(source.Data) > maxVertexFrameBytes {
			return media.VideoRequest{}, p.failure(
				media.ErrorUnsupported,
				"Vertex Veo first frame exceeds 20 MB",
			)
		}
		instance["image"] = map[string]any{
			"bytesBase64Encoded": base64.StdEncoding.EncodeToString(source.Data),
			"mimeType":           source.MIMEType,
		}
	}
	// One sample means one video and one charge.
	parameters := map[string]any{"sampleCount": 1}
	// Veo 2 rejects generateAudio; every later model takes it.
	if !strings.HasPrefix(in.Model, "veo-2") {
		parameters["generateAudio"] = !in.Settings.DisableAudio
	}
	if in.Settings.AspectRatio != "" {
		parameters["aspectRatio"] = in.Settings.AspectRatio
	}
	if in.Settings.Duration != 0 {
		parameters["durationSeconds"] = int(in.Settings.Duration / time.Second)
	}
	if in.Settings.Resolution != "" {
		parameters["resolution"] = in.Settings.Resolution
	}
	body, err := json.Marshal(map[string]any{
		"instances":  []any{instance},
		"parameters": parameters,
	})
	if err != nil {
		return media.VideoRequest{}, err
	}
	return media.NewVideoRequest(media.VideoRequestSnapshot{
		Origin:           p.origin(in.Model),
		Input:            in,
		Effective:        in.Settings,
		Body:             body,
		ContentType:      "application/json",
		ModelInfo:        info,
		CapabilityPolicy: p.config.CapabilityPolicy,
		Unverified:       unverified,
	})
}

// SubmitVideo sends the prepared job once. A missing credential returns
// SubmissionRejected without sending; a transport failure or server-side
// ambiguity returns SubmissionUnknown, which does not authorize a resubmit.
func (p *VertexVideoJobClient) SubmitVideo(
	ctx context.Context,
	prepared media.VideoRequest,
) (media.Submission, error) {
	in := prepared.Input()
	if prepared.Origin() != p.origin(in.Model) ||
		prepared.ContentType() != "application/json" {
		return media.Submission{
				State: media.SubmissionRejected,
			}, p.failure(
				media.ErrorInvalidRequest,
				"prepared video request belongs to another destination",
			)
	}
	header, err := p.header(ctx)
	if err != nil {
		return media.Submission{State: media.SubmissionRejected}, err
	}
	raw, err := doGeminiMedia(
		ctx,
		p.Provider(),
		header,
		p.client,
		http.MethodPost,
		p.videoURL(in.Model),
		prepared.Body(),
		prepared.ContentType(),
		maxOperationBytes,
		false,
	)
	if err != nil {
		providerErr, ok := errors.AsType[*media.Error](err)
		if ok && providerErr.Status >= 400 &&
			providerErr.Status < 500 &&
			providerErr.Status != 408 &&
			providerErr.Status != 409 &&
			providerErr.Status != 425 && providerErr.Status != 429 {
			return media.Submission{State: media.SubmissionRejected}, err
		}
		return media.Submission{State: media.SubmissionUnknown}, err
	}
	var response struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.Name == "" {
		return media.Submission{
				State: media.SubmissionUnknown,
			}, p.failure(
				media.ErrorProtocol,
				"Vertex Veo accepted response has no operation name: %v",
				err,
			)
	}
	if err := p.checkOperation(response.Name); err != nil {
		return media.Submission{State: media.SubmissionUnknown}, err
	}
	job := media.Job{
		ID:     response.Name,
		Kind:   media.KindVideo,
		Origin: p.origin(in.Model),
	}
	return media.Submission{State: media.SubmissionAccepted, Job: &job}, nil
}

// PollVideo reads one operation status and never resubmits the job. A
// succeeded poll carries the video bytes, up to tens of MB at 4k: persist the
// Job for restart, not a succeeded poll, and store the bytes once, from
// FetchVideo's result.
func (p *VertexVideoJobClient) PollVideo(
	ctx context.Context,
	job media.Job,
) (media.JobPoll, error) {
	if job.Kind != media.KindVideo || job.Origin != p.origin(job.Origin.Model) {
		return media.JobPoll{}, p.failure(
			media.ErrorInvalidRequest,
			"video job belongs to another destination",
		)
	}
	if err := p.checkOperation(job.ID); err != nil {
		return media.JobPoll{}, err
	}
	header, err := p.header(ctx)
	if err != nil {
		return media.JobPoll{}, err
	}
	body, _ := json.Marshal(map[string]string{"operationName": job.ID})
	// The poll URL always comes from the configuration, so an operation name
	// cannot send the token elsewhere.
	raw, err := doGeminiMedia(
		ctx,
		p.Provider(),
		header,
		p.client,
		http.MethodPost,
		strings.TrimSuffix(p.videoURL(job.Origin.Model), ":predictLongRunning")+
			":fetchPredictOperation",
		body,
		"application/json",
		maxVertexOperationBytes,
		false,
	)
	if err != nil {
		return media.JobPoll{}, err
	}
	var response struct {
		Name  string `json:"name"`
		Done  bool   `json:"done"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Response struct {
			RAIMediaFilteredCount   int      `json:"raiMediaFilteredCount"`
			RAIMediaFilteredReasons []string `json:"raiMediaFilteredReasons"`
			Videos                  []struct {
				BytesBase64Encoded []byte `json:"bytesBase64Encoded"`
				GCSURI             string `json:"gcsUri"`
				MIMEType           string `json:"mimeType"`
			} `json:"videos"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return media.JobPoll{}, p.failure(
			media.ErrorProtocol,
			"decode Vertex Veo operation: %w",
			err,
		)
	}
	if response.Name != "" && response.Name != job.ID {
		return media.JobPoll{}, p.failure(
			media.ErrorProtocol,
			"Vertex Veo operation identity changed",
		)
	}
	poll := media.JobPoll{Job: job.Clone(), State: media.JobPending}
	if !response.Done {
		return poll, nil
	}
	if response.Error != nil {
		poll.State = media.JobFailed
		poll.Failure = cmp.Or(
			response.Error.Message,
			fmt.Sprintf("Vertex Veo operation failed with code %d", response.Error.Code),
		)
		return poll, nil
	}
	for _, video := range response.Response.Videos {
		if len(video.BytesBase64Encoded) == 0 {
			// Dashi never sets storageUri, so a Cloud Storage output is
			// unexpected and Dashi holds no credential scoped to read it.
			return media.JobPoll{}, p.failure(
				media.ErrorProtocol,
				"Vertex Veo output has no inline bytes (gcsUri %q)",
				video.GCSURI,
			)
		}
		mimeType := cmp.Or(video.MIMEType, "video/mp4")
		if !strings.HasPrefix(mimeType, "video/") {
			return media.JobPoll{}, p.failure(
				media.ErrorProtocol,
				"Vertex Veo output has non-video MIME type %q",
				mimeType,
			)
		}
		poll.Content = append(poll.Content, content.VideoBlock(content.Source{
			Data:     video.BytesBase64Encoded,
			MIMEType: mimeType,
		}))
	}
	if len(poll.Content) == 0 {
		if response.Response.RAIMediaFilteredCount > 0 {
			poll.State = media.JobFailed
			poll.Failure = "blocked by safety filters: " +
				strings.Join(response.Response.RAIMediaFilteredReasons, "; ")
			return poll, nil
		}
		return media.JobPoll{}, p.failure(
			media.ErrorProtocol,
			"completed Vertex Veo job has no generated video",
		)
	}
	poll.State = media.JobSucceeded
	return poll, nil
}

// FetchVideo returns the videos from a succeeded poll and sends nothing. It
// measures each video from the MP4 file: Video.Duration and a
// Result.VideoUsage entry. A video it cannot measure is still returned,
// without either; bill it from the recorded request.
func (p *VertexVideoJobClient) FetchVideo(
	ctx context.Context,
	poll media.JobPoll,
) (media.Result, error) {
	if err := ctx.Err(); err != nil {
		return media.Result{}, err
	}
	if err := poll.Validate(); err != nil {
		return media.Result{}, p.failure(media.ErrorInvalidRequest, "%w", err)
	}
	job := poll.Job
	if poll.State != media.JobSucceeded || job.Kind != media.KindVideo ||
		job.Origin != p.origin(job.Origin.Model) || len(poll.Content) == 0 {
		return media.Result{}, p.failure(
			media.ErrorInvalidRequest,
			"fetch requires a succeeded Vertex Veo poll",
		)
	}
	result := media.Result{
		Kind:       media.KindVideo,
		Requested:  model.ModelRef{Provider: p.Provider(), Model: job.Origin.Model},
		API:        VideoAPI,
		ResponseID: job.ID,
	}
	for i, block := range poll.Content {
		if block.Video == nil || len(block.Video.Source.Data) == 0 {
			return media.Result{}, p.failure(
				media.ErrorInvalidRequest,
				"Vertex Veo poll output has no inline video",
			)
		}
		output := content.VideoBlock(content.Source{
			Data:     slices.Clone(block.Video.Source.Data),
			MIMEType: block.Video.Source.MIMEType,
		})
		if info, err := mp4.Probe(output.Video.Source.Data); err == nil {
			output.Video.Duration = info.Duration
			result.VideoUsage = append(result.VideoUsage, media.VideoUsage{
				ContentIndex: i,
				Duration:     info.Duration,
				Width:        info.Width,
				Height:       info.Height,
				Resolution:   videoResolution(info.Width, info.Height),
				Audio:        info.Audio,
			})
		}
		result.Content = append(result.Content, output)
	}
	if err := result.Validate(); err != nil {
		return media.Result{}, p.failure(media.ErrorProtocol, "%w", err)
	}
	return result, nil
}

// videoResolution names the tier of the shorter edge, or "" for other sizes.
func videoResolution(width, height int) string {
	switch min(width, height) {
	case 720:
		return "720p"
	case 1080:
		return "1080p"
	case 2160:
		return "4k"
	}
	return ""
}

// header returns the request credential headers. With a configured Client it
// adds none. It detects ADC on first use and keeps it; a failed detection is
// not kept, so the next send tries again. auth.Credentials caches and
// refreshes the token.
func (p *VertexVideoJobClient) header(ctx context.Context) (http.Header, error) {
	if !p.adc {
		return nil, nil
	}
	p.mu.Lock()
	credentials := p.credentials
	if credentials == nil {
		var err error
		credentials, err = p.detect()
		if err != nil {
			p.mu.Unlock()
			return nil, p.failure(
				media.ErrorAuthentication,
				"find Application Default Credentials: %w",
				err,
			)
		}
		p.credentials = credentials
	}
	p.mu.Unlock()
	token, err := credentials.Token(ctx)
	if err != nil {
		return nil, p.failure(media.ErrorAuthentication, "get access token: %w", err)
	}
	header := http.Header{}
	header.Set("Authorization", cmp.Or(token.Type, "Bearer")+" "+token.Value)
	// User credentials need a quota project for Vertex, as in Config.
	quota, err := credentials.QuotaProjectID(ctx)
	if err != nil {
		return nil, p.failure(media.ErrorAuthentication, "get quota project: %w", err)
	}
	if quota != "" {
		header.Set("X-Goog-User-Project", quota)
	}
	return header, nil
}

// checkOperation accepts only
// projects/{project}/locations/{location}/publishers/google/models/{model}/operations/{id}.
// The location must match. The project, model, and ID are checked by format
// only: Vertex may name the project by number or the model by the ID it
// resolved, and a paid job must stay pollable. The name only goes into the
// poll's JSON body; the poll URL always comes from the configuration.
func (p *VertexVideoJobClient) checkOperation(operation string) error {
	parts := strings.Split(operation, "/")
	if len(parts) != 10 ||
		parts[0] != "projects" || !projectSegment(parts[1]) ||
		parts[2] != "locations" || parts[3] != p.config.Location ||
		parts[4] != "publishers" || parts[5] != "google" ||
		parts[6] != "models" || !modelSegment(parts[7]) ||
		parts[8] != "operations" || !operationSegment(parts[9]) {
		return p.failure(
			media.ErrorProtocol,
			"Vertex Veo operation %q does not belong to this location",
			operation,
		)
	}
	return nil
}

// projectSegment accepts a project ID or number.
func projectSegment(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(c rune) bool {
		return c != '-' && c != '_' && !isASCIIAlnum(c)
	})
}

// modelSegment accepts a model ID such as veo-3.1-generate-001 or
// imagegeneration@006.
func modelSegment(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(c rune) bool {
		return c != '-' && c != '_' && c != '.' && c != '@' && !isASCIIAlnum(c)
	})
}

// operationSegment accepts any printable ASCII ID without spaces; the split
// on "/" already removed slashes.
func operationSegment(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(c rune) bool {
		return c <= ' ' || c > '~'
	})
}

func isASCIIAlnum(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func (p *VertexVideoJobClient) videoURL(name string) string {
	return strings.ReplaceAll(p.config.Endpoint, "{model}", url.PathEscape(name))
}

func (p *VertexVideoJobClient) origin(name string) content.Origin {
	scopeData, _ := json.Marshal([]string{
		VideoAPI,
		p.videoURL(name),
		p.config.CredentialScope,
	})
	sum := sha256.Sum256(scopeData)
	return content.Origin{
		Provider: p.Provider(),
		API:      VideoAPI,
		Model:    name,
		Scope:    fmt.Sprintf("%x", sum),
	}
}

func (p *VertexVideoJobClient) failure(
	category media.ErrorCategory,
	format string,
	args ...any,
) error {
	return mediaFailure(p.Provider(), category, format, args...)
}
