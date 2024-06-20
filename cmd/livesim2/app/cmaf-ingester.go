package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Eyevinn/dash-mpd/mpd"
	"github.com/Eyevinn/mp4ff/bits"
)

type CmafIngesterRequest struct {
	User      string `json:"user"`
	PassWord  string `json:"password"`
	Dest      string `json:"destination"`
	URL       string `json:"livesimURL"`
	TestNowMS *int   `json:"testTimeMS,omitempty"`
}

type ingesterState int

const (
	ingesterStateNotStarted ingesterState = iota
	ingesterStateRunning
	ingesterStateStopped
)

type cmafIngesterMgr struct {
	nr        atomic.Uint64
	ingesters map[uint64]*cmafIngester
	state     ingesterState
	s         *Server
}

func NewCmafIngesterMgr(s *Server) *cmafIngesterMgr {
	return &cmafIngesterMgr{
		ingesters: make(map[uint64]*cmafIngester),
		state:     ingesterStateNotStarted,
		s:         s,
	}
}

func (cm *cmafIngesterMgr) Start() {
	cm.state = ingesterStateRunning
}

func (cm *cmafIngesterMgr) NewCmafIngester(req CmafIngesterRequest) (nr uint64, err error) {
	if cm.state != ingesterStateRunning {
		return 0, fmt.Errorf("CMAF ingester manager not running")
	}
	for { // Get unique atomic number
		prev := cm.nr.Load()
		nr = prev + 1
		if cm.nr.CompareAndSwap(prev, nr) {
			break
		}
	}

	log := slog.Default().With(slog.Uint64("ingester", nr))

	mpdReq := httptest.NewRequest("GET", req.URL, nil)
	if req.TestNowMS != nil {
		mpdReq.URL.RawQuery = fmt.Sprintf("nowMS=%d", *req.TestNowMS)
	}
	nowMS, cfg, errHT := cfgFromRequest(mpdReq, log)
	if errHT != nil {
		return 0, fmt.Errorf("failed to get config from request: %w", errHT)
	}

	contentPart := cfg.URLContentPart()
	log.Debug("CMAF ingest content", "url", contentPart)
	asset, ok := cm.s.assetMgr.findAsset(contentPart)
	if !ok {
		return 0, fmt.Errorf("unknown asset %q", contentPart)
	}
	_, mpdName := path.Split(contentPart)
	liveMPD, err := LiveMPD(asset, mpdName, cfg, nowMS)
	if err != nil {
		return 0, fmt.Errorf("failed to generate live MPD: %w", err)
	}

	// Extract list of all representations with their information

	period := liveMPD.Periods[0]
	nrReps := 0
	for _, a := range period.AdaptationSets {
		nrReps += len(a.Representations)
	}

	repsData := make([]cmafRepData, 0, nrReps)

	adaptationSets := orderAdaptationSetsByContentType(period.AdaptationSets)

	for _, a := range adaptationSets {
		contentType := a.ContentType
		var mimeType string
		switch contentType {
		case "video":
			mimeType = "video/mp4"
		case "audio":
			mimeType = "audio/mp4"
		case "text":
			mimeType = "application/mp4"
		default:
			return 0, fmt.Errorf("unknown content type: %s", contentType)
		}
		for _, r := range a.Representations {
			// TODO. Add relevant BaseURLs from MPD if present
			segTmpl := r.GetSegmentTemplate()
			rd := cmafRepData{
				repID:        r.Id,
				contentType:  string(contentType),
				mimeType:     mimeType,
				initPath:     replaceIdentifiers(r, segTmpl.Initialization),
				mediaPattern: replaceIdentifiers(r, segTmpl.Media),
			}
			repsData = append(repsData, rd)
		}
	}

	c := cmafIngester{
		mgr:            cm,
		user:           req.User,
		passWord:       req.PassWord,
		dest:           req.Dest,
		url:            req.URL,
		TestNowMS:      req.TestNowMS,
		log:            log,
		cfg:            cfg,
		asset:          asset,
		repsData:       repsData,
		nextSegTrigger: make(chan struct{}),
	}
	cm.ingesters[nr] = &c

	return nr, nil
}

type cmafRepData struct {
	repID        string
	contentType  string
	mimeType     string
	initPath     string
	mediaPattern string
}

type cmafIngester struct {
	mgr            *cmafIngesterMgr
	user           string
	passWord       string
	dest           string
	url            string
	log            *slog.Logger
	TestNowMS      *int
	cfg            *ResponseConfig
	asset          *asset
	repsData       []cmafRepData
	nextSegTrigger chan struct{}
	report         []string
}

// start starts the main ingest loop for sending init and media packets.
// It calculates the availability time for the next segment and
// then waits until that time to send all segments.
// Segments are sent in parallel for all representation, as
// low-latency mode requires parallel chunked-transfer encoding streams.
// If TestNowMS is set, the ingester runs in test mode, and
// the time is not taken from the system clock, but start from
// TestNowMS and increments to next segment every time testNextSegment()
// is called.
func (c *cmafIngester) start(ctx context.Context) {

	// Finally we should send off the init segments
	// and then start the loop for sending the media segments

	var initBin []byte
	contentType := "application/mp4"
	for _, rd := range c.repsData {
		prefix, lang, ok, err := matchTimeSubsInitLang(c.cfg, rd.initPath)
		if ok {
			if err != nil {
				msg := fmt.Sprintf("error matching time subs init lang: %v", err)
				c.report = append(c.report, msg)
				c.log.Error(msg)
				return
			}
			init := createTimeSubsInitSegment(prefix, lang, SUBS_TIME_TIMESCALE)
			sw := bits.NewFixedSliceWriter(int(init.Size()))
			err := init.EncodeSW(sw)
			if err != nil {
				msg := fmt.Sprintf("Error encoding init segment: %v", err)
				c.report = append(c.report, msg)
				c.log.Error(msg)
				return
			}
			initBin = sw.Bytes()
		} else {
			match, err := matchInit(rd.initPath, c.cfg, c.asset)
			if err != nil {
				msg := fmt.Sprintf("Error matching init segment: %v", err)
				c.report = append(c.report, msg)
				c.log.Error(msg)
			}
			if !match.isInit {
				msg := fmt.Sprintf("Error matching init segment: %v", err)
				c.report = append(c.report, msg)
				c.log.Error(msg)
			}
			contentType = match.rep.SegmentType()
			initBin = match.init

			assetParts := c.cfg.URLParts[2 : len(c.cfg.URLParts)-1]
			initPathParts := append(assetParts, rd.initPath)
			initPath := strings.Join(initPathParts, "/")

			err = c.sendInitSegment(ctx, initPath, rd.mimeType, initBin)
			if err != nil {
				msg := fmt.Sprintf("error uploading init segment: %v", err)
				c.report = append(c.report, msg)
				c.log.Error(msg)
			}

		}
		c.log.Info("Sending init segment", "path", rd.initPath, "contentType", contentType, "size", len(initBin))
	}

	// Now calculate the availability time for the next segment
	var nowMS int
	if c.TestNowMS != nil {
		nowMS = *c.TestNowMS
	} else {
		nowMS = int(time.Now().UnixNano() / 1e6)
	}

	refRep := c.asset.refRep
	lastNr := findLastSegNr(c.cfg, c.asset, nowMS, refRep)
	nextSegNr := lastNr + 1
	c.log.Debug("Next segment number at start", "nr", nextSegNr)
	availabilityTime, err := calcSegmentAvailabilityTime(c.asset, refRep, uint32(nextSegNr), c.cfg)
	if err != nil {
		msg := fmt.Sprintf("Error calculating segment availability time: %v", err)
		c.report = append(c.report, msg)
		c.log.Error(msg)
		return
	}
	c.log.Info("Next segment availability time", "time", availabilityTime)
	var timer *time.Timer
	deltaTime := 24 * time.Hour
	if c.TestNowMS == nil {
		deltaTime = time.Duration(availabilityTime-int64(nowMS)) * time.Millisecond
	}
	timer = time.NewTimer(deltaTime)
	defer func() {
		if !timer.Stop() {
			<-timer.C
		}
	}()

	// Main loop for sending segments
	for {
		c.log.Info("Waiting for next segment")
		select {
		case <-timer.C:
			// Send next segment
		case <-c.nextSegTrigger:
			// Send next segment
		case <-ctx.Done():
			c.log.Info("Context done, stopping ingest")
			return
		}
		err := c.sendMediaSegments(ctx, nextSegNr, int(availabilityTime))
		if err != nil {
			msg := fmt.Sprintf("Error sending media segments: %v", err)
			c.report = append(c.report, msg)
			c.log.Error(msg)
			return
		}
		nextSegNr++
		availabilityTime, err = calcSegmentAvailabilityTime(c.asset, refRep, uint32(nextSegNr), c.cfg)
		if err != nil {
			msg := fmt.Sprintf("Error calculating segment availability time: %v", err)
			c.report = append(c.report, msg)
			c.log.Error(msg)
			return
		}
		if c.TestNowMS == nil {
			nowMS = int(time.Now().UnixNano() / 1e6)
		}

		c.log.Info("Next segment availability time", "time", availabilityTime)
		if c.TestNowMS == nil {
			deltaTime := time.Duration(availabilityTime-int64(nowMS)) * time.Millisecond
			for {
				if deltaTime > 0 {
					break
				}
				msg := fmt.Sprintf("Segment availability time in the past: %d", availabilityTime)
				c.report = append(c.report, msg)
				c.log.Error(msg)
				err := c.sendMediaSegments(ctx, nextSegNr, int(availabilityTime))
				if err != nil {
					msg := fmt.Sprintf("Error sending media segments: %v", err)
					c.report = append(c.report, msg)
					c.log.Error(msg)
					return
				}
				nextSegNr++
				availabilityTime, err = calcSegmentAvailabilityTime(c.asset, refRep, uint32(nextSegNr), c.cfg)
				if err != nil {
					msg := fmt.Sprintf("Error calculating segment availability time: %v", err)
					c.report = append(c.report, msg)
					c.log.Error(msg)
					return
				}
				nowMS = int(time.Now().UnixNano() / 1e6)
				deltaTime = time.Duration(availabilityTime-int64(nowMS)) * time.Millisecond
			}
			timer.Reset(deltaTime)
		}
	}

	// connect to URL
	// if user != "", do basic authentication
	// Use URL to get an MPD from internal engine
	// Generate init segments as described in MPD
	// Do HTTP PUT for each init segment
	// Then calculate next segment number and pause/sleep until time to send it.
	// Loop:
	//    Calculate time for next segment, and set timer
	//    At timer, push all generated segments (all representations)
	//    Count how many segments have been pushed, and stop
	//    if limit is passed.
	//    Note, for low-latency, one needs parallel HTTP sessions
	//    in H1/H2. There therefore need to be as many HTTP sessions
	//    to the same host as there are representations pushed.
	//
	// Error handling:
	//    If getting behind in time or not successful
	//        gather statistics into own report
	//    The upload client (HTTP client) should have timeout.
	// Stopping:
	// There should be a context so that one can cancel this loop
	//    * Either triggered by shutting down the server, or by REST DELETE
	//    * If DELETE, one should get a report back
	//    * Any ongoing uploads should ideally finish before stopping
	//      so that all representations are synchronized and have the same
	//      number of segments
	//
	// Reporting:
	//    * It should be possible to ask for a report by sending a GET request
	//    * DELETE should also return a report of what has been sent
	//
	// CMAF-Ingest interface
	//
	//    * Interface #1 may be to only send segments
	//    * The metadata then need to be added like role in `kind` boxes`, but also prft
	//    * Sending an MPD would help
	//
	//    * SCTE-35 events should be sent as a separate event stream. This will mostly have
	//      empty segments. Should check what AWS is outputting to get a reference

	//
}

func (c *cmafIngester) triggerNextSegment() {
	c.nextSegTrigger <- struct{}{}
}

func (c *cmafIngester) sendInitSegment(ctx context.Context, filename, mimeType string, data []byte) error {
	url := fmt.Sprintf("%s/%s", c.dest, filename)
	buf := bytes.NewBuffer(data)
	req, err := http.NewRequestWithContext(ctx, "PUT", url, buf)
	if err != nil {
		return fmt.Errorf("Error creating request: %w", err)
	}
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("Connection", "keep-alive")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Error sending request: %w", err)
	}
	_, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("Error reading response body: %w", err)
	}
	defer func() {
		slog.Debug("Closing body", "filename", filename)
		resp.Body.Close()
	}()
	return nil
}

func (c *cmafIngester) sendMediaSegments(ctx context.Context, nextSegNr, nowMS int) error {
	c.log.Debug("Start media segment", "nr", nextSegNr, "nowMS", nowMS)
	assetParts := c.cfg.URLParts[2 : len(c.cfg.URLParts)-1]
	wTimes := calcWrapTimes(c.asset, c.cfg, nowMS+50, mpd.Duration(100*time.Millisecond))
	wg := sync.WaitGroup{}
	if c.cfg.SegTimelineFlag {
		var segPart string
		var refSegEntries segEntries
		atoMS := int(c.cfg.getAvailabilityTimeOffsetS() * 1000)
		for idx, rd := range c.repsData {
			var se segEntries
			// The first representation is used as reference for generating timeline entries
			if idx == 0 {
				refSegEntries = c.asset.generateTimelineEntries(rd.repID, wTimes, atoMS)
				se = refSegEntries
			} else {
				switch rd.contentType {
				case "video", "text", "image":
					se = c.asset.generateTimelineEntries(rd.repID, wTimes, atoMS)
				case "audio":
					se = c.asset.generateTimelineEntriesFromRef(refSegEntries, rd.repID)
				default:
					return fmt.Errorf("unknown content type %s", rd.contentType)
				}
			}
			segTime := int(se.lastTime())
			segPart = replaceTimeOrNr(rd.mediaPattern, segTime)
			segPath := strings.Join(append(assetParts, segPart), "/")
			wg.Add(1)
			go c.sendMediaSegment(ctx, &wg, segPath, segPart, nextSegNr, nowMS, rd)
		}
	} else {
		for _, rd := range c.repsData {
			segPart := replaceTimeOrNr(rd.mediaPattern, nextSegNr)
			segPath := strings.Join(append(assetParts, segPart), "/")
			wg.Add(1)
			go c.sendMediaSegment(ctx, &wg, segPath, segPart, nextSegNr, nowMS, rd)
		}
	}
	wg.Wait()
	return nil
}

func (c *cmafIngester) sendMediaSegment(ctx context.Context, wg *sync.WaitGroup, segPath, segPart string, segNr, nowMS int, rd cmafRepData) {
	defer wg.Done()
	stopCh := make(chan struct{})
	finishedCh := make(chan struct{})
	defer close(stopCh)
	defer close(finishedCh)

	u := c.dest + "/" + segPath
	c.log.Info("send media segment", "path", segPath, "segNr", segNr, "nowMS", nowMS, "url", u)

	src := newCmafSource(stopCh, finishedCh, c.log, u)
	go src.start(ctx)

	// Create media segment based on number and send it to segPath
	c.log.Debug("Sending media segment", "path", segPath, "segNr", segNr, "nowMS", nowMS)
	code, err := writeSegment(ctx, src, c.log, c.cfg, c.mgr.s.assetMgr.vodFS, c.asset, segPart, nowMS, c.mgr.s.textTemplates)
	if err != nil {
		c.log.Error("writeSegment", "code", code, "err", err)
		var tooEarly errTooEarly
		switch {
		case errors.Is(err, errNotFound):
			c.log.Error("segment not found", "path", segPath)
			return
		case errors.As(err, &tooEarly):
			c.log.Error("segment too early", "path", segPath)
			return
		case errors.Is(err, errGone):
			c.log.Error("segment gone", "path", segPath)
			return
		default:
			c.log.Error("writeSegment", "err", err)
			http.Error(src, "writeSegment", http.StatusInternalServerError)
			return
		}
	}
	stopCh <- struct{}{}
	<-finishedCh
}

// cmafSource intermediates HTTP response writer and client push writer
// It provides a Read method that the client can use to read the data.
type cmafSource struct {
	mu         sync.Mutex
	ctx        context.Context
	noMoreCh   chan struct{}
	finishedCh chan struct{}
	moreDataCh chan struct{}
	url        string
	h          http.Header
	status     int
	log        *slog.Logger
	buf        []byte
}

func newCmafSource(noMoreCh, finishedCh chan struct{}, log *slog.Logger, url string) *cmafSource {
	cs := cmafSource{
		noMoreCh:   noMoreCh,
		finishedCh: finishedCh,
		moreDataCh: make(chan struct{}),
		url:        url,
		h:          make(http.Header),
		log:        log,
		buf:        make([]byte, 0, 1024*1024),
	}
	return &cs
}

func (c *cmafSource) start(ctx context.Context) {

	c.ctx = ctx
	req, err := http.NewRequestWithContext(ctx, "PUT", c.url, c)
	defer func() {
		c.finishedCh <- struct{}{}
	}()
	if err != nil {
		c.log.Error("creating request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Connection", "keep-alive")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.log.Error("creating request", "err", err)
		return
	}
	_, err = io.ReadAll(resp.Body)
	if err != nil {
		c.log.Warn("Error reading response body", "err", err)
	}
	defer func() {
		c.log.Debug("Closing body", "url", c.url)
		resp.Body.Close()
	}()
}

func (c *cmafSource) Header() http.Header {
	return c.h
}

func (c *cmafSource) Flush() {
	c.log.Debug("Flush")
}

func (c *cmafSource) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log.Debug("Write", "url", c.url, "len", len(b))
	c.buf = append(c.buf, b...)
	return len(b), nil
}

func (c *cmafSource) WriteHeader(status int) {
	c.log.Debug("Writer status", "status", status)
	c.status = status
}

func (c *cmafSource) Read(p []byte) (int, error) {
	i := 0
	for {
		c.mu.Lock()
		if i%10 == 0 {
			c.log.Debug("Read", "len", len(c.buf), "i", i)
		}
		i++
		if len(c.buf) > 0 {
			n := copy(p, c.buf)
			if n < len(c.buf) {
				c.buf = c.buf[n:]
			} else {
				c.buf = c.buf[:0]
			}
			c.mu.Unlock()
			return n, nil
		}
		c.mu.Unlock()
		select {
		case <-c.ctx.Done():
			return 0, io.EOF
		case <-c.noMoreCh:
			return 0, io.EOF
		default:
			time.Sleep(250 * time.Millisecond)
		}
	}
}
