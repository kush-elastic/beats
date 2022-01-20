// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package httpjson

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"

	inputcursor "github.com/elastic/beats/v7/filebeat/input/v2/input-cursor"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/logp"
)

const requestNamespace = "request"

func registerRequestTransforms() {
	registerTransform(requestNamespace, appendName, newAppendRequest)
	registerTransform(requestNamespace, deleteName, newDeleteRequest)
	registerTransform(requestNamespace, setName, newSetRequestPagination)
}

type httpClient struct {
	client  *http.Client
	limiter *rateLimiter
}

func (c *httpClient) do(stdCtx context.Context, trCtx *transformContext, req *http.Request) (*http.Response, error) {
	resp, err := c.limiter.execute(stdCtx, func() (*http.Response, error) {
		return c.client.Do(req)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to execute http client.Do: %w", err)
	}
	if resp.StatusCode > 399 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("server responded with status code %d: %s", resp.StatusCode, string(body))
	}
	return resp, nil
}

func (rf *requestFactory) newRequest(ctx *transformContext) (transformable, error) {
	req := transformable{}
	req.setURL(rf.url)

	if rf.body != nil && len(*rf.body) > 0 {
		req.setBody(rf.body.Clone())
	}

	header := http.Header{}
	header.Set("Accept", "application/json")
	header.Set("User-Agent", userAgent)
	req.setHeader(header)

	var err error
	for _, t := range rf.transforms {
		req, err = t.run(ctx, req)
		if err != nil {
			return transformable{}, err
		}
	}

	if rf.method == "POST" {
		header = req.header()
		if header.Get("Content-Type") == "" {
			header.Set("Content-Type", "application/json")
			req.setHeader(header)
		}
	}

	rf.log.Debugf("new request: %#v", req)

	return req, nil
}

type requestFactory struct {
	url        url.URL
	method     string
	body       *common.MapStr
	transforms []basicTransform
	user       string
	password   string
	log        *logp.Logger
	encoder    encoderFunc
	replace    *regexp.Regexp
	split      *split
}

func newRequestFactory(config config, log *logp.Logger) []*requestFactory {
	// config validation already checked for errors here
	var rfs []*requestFactory
	ts, _ := newBasicTransformsFromConfig(config.Request.Transforms, requestNamespace, log)
	// regular call requestFactory object
	rf := &requestFactory{
		url:        *config.Request.URL.URL,
		method:     config.Request.Method,
		body:       config.Request.Body,
		transforms: ts,
		log:        log,
		encoder:    registeredEncoders[config.Request.EncodeAs],
	}
	if config.Auth != nil && config.Auth.Basic.isEnabled() {
		rf.user = config.Auth.Basic.User
		rf.password = config.Auth.Basic.Password
	}
	rfs = append(rfs, rf)
	for _, ch := range config.Chain {
		reg, _ := regexp.Compile(ch.Step.Replace)
		// chain calls requestFactory object
		split, _ := newSplitResponse(ch.Step.Response.Split, log)
		rf := &requestFactory{
			url:        *ch.Step.Request.URL.URL,
			method:     ch.Step.Request.Method,
			body:       ch.Step.Request.Body,
			transforms: ts,
			log:        log,
			encoder:    registeredEncoders[config.Request.EncodeAs],
			replace:    reg,
			split:      split,
		}
		rfs = append(rfs, rf)
	}
	return rfs
}

func (rf *requestFactory) newHTTPRequest(stdCtx context.Context, trCtx *transformContext) (*http.Request, error) {
	trReq, err := rf.newRequest(trCtx)
	if err != nil {
		return nil, err
	}

	var body []byte
	if rf.method == "POST" {
		if rf.encoder != nil {
			body, err = rf.encoder(trReq)
		} else {
			body, err = encode(trReq.header().Get("Content-Type"), trReq)
		}
		if err != nil {
			return nil, err
		}
	}

	url := trReq.url()
	req, err := http.NewRequest(rf.method, url.String(), bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	req = req.WithContext(stdCtx)

	req.Header = trReq.header().Clone()

	if rf.user != "" || rf.password != "" {
		req.SetBasicAuth(rf.user, rf.password)
	}

	return req, nil
}

type requester struct {
	log               *logp.Logger
	client            *httpClient
	requestFactories  []*requestFactory
	responseProcessor *responseProcessor
}

func newRequester(
	client *httpClient,
	requestFactory []*requestFactory,
	responseProcessor *responseProcessor,
	log *logp.Logger) *requester {
	return &requester{
		log:               log,
		client:            client,
		requestFactories:  requestFactory,
		responseProcessor: responseProcessor,
	}
}

// collectResponse returns response from provided request
func (rf *requestFactory) collectResponse(stdCtx context.Context, trCtx *transformContext, r *requester, publisher inputcursor.Publisher) (*http.Response, error) {
	req, err := rf.newHTTPRequest(stdCtx, trCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	httpResp, err := r.client.do(stdCtx, trCtx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute http client.Do: %w", err)
	}
	return httpResp, nil
}

// generateNewUrl returns new url value using replacement from oldUrl with ids
func generateNewUrl(replacement *regexp.Regexp, oldUrl, ids string) (url.URL, error) {
	newUrl, err := url.Parse(replacement.ReplaceAllString(oldUrl, ids))
	if err != nil {
		return url.URL{}, fmt.Errorf("failed to replace value in url: %w", err)
	}
	return *newUrl, nil

}

func (r *requester) doRequest(stdCtx context.Context, trCtx *transformContext, publisher inputcursor.Publisher) error {
	var (
		n                 int
		ids               []string
		err               error
		split             *split
		urlCopy           url.URL
		urlString         string
		httpResp          *http.Response
		intermediateResps []*http.Response
		finalResps        []*http.Response
	)
	rfSize := len(r.requestFactories)
	for i, rf := range r.requestFactories {
		// iterate over collected ids from last response
		if i == 0 {
			// perform and store regular call responses
			httpResp, err = rf.collectResponse(stdCtx, trCtx, r, publisher)
			if err != nil {
				return fmt.Errorf("failed to execute rf.collectResponse: %w", err)
			}
			if rfSize == 1 {
				finalResps = append(finalResps, httpResp)
				n, err = r.processAndPublishEvents(stdCtx, trCtx, publisher, finalResps, true)
				if err != nil {
					return err
				}
				continue
			} else {
				intermediateResps = append(intermediateResps, httpResp)
				ids, err = r.getIdsFromResponses(intermediateResps, r.requestFactories[i+1].replace)
				if err != nil {
					return err
				}
				n, err = r.processAndPublishEvents(stdCtx, trCtx, publisher, intermediateResps, false)
				if err != nil {
					return err
				}
			}
		} else {
			if len(ids) == 0 {
				n = 0
				continue
			}
			urlCopy = rf.url
			urlString = rf.url.String()
			// perform request over collected ids
			for _, id := range ids {
				// reformat urls of requestFactory using ids
				rf.url, err = generateNewUrl(rf.replace, urlString, id)
				if err != nil {
					return fmt.Errorf("failed to generate new URL: %w", err)
				}

				// collect data from new urls
				httpResp, err = rf.collectResponse(stdCtx, trCtx, r, publisher)
				if err != nil {
					return fmt.Errorf("failed to execute rf.collectResponse: %w", err)
				}
				// store data according to response type
				if i == rfSize-1 && len(ids) != 0 {
					finalResps = append(finalResps, httpResp)
				} else {
					intermediateResps = append(intermediateResps, httpResp)
				}
			}
			rf.url = urlCopy

			if i == rfSize-1 {
				split = r.responseProcessor.split
				r.responseProcessor.split = rf.split
				n, err = r.processAndPublishEvents(stdCtx, trCtx, publisher, finalResps, true)
				if err != nil {
					return err
				}
				r.responseProcessor.split = split
			} else {
				// The if comdition (i == rfSize-1) ensures this branch never runs to the last element
				// of r.requestFactories, therefore r.requestFactories[i+1] will never be out of bounds.
				ids, err = r.getIdsFromResponses(intermediateResps, r.requestFactories[i+1].replace)
				if err != nil {
					return err
				}
				split = r.responseProcessor.split
				r.responseProcessor.split = rf.split
				n, err = r.processAndPublishEvents(stdCtx, trCtx, publisher, intermediateResps, false)
				if err != nil {
					return err
				}
				r.responseProcessor.split = split
			}
		}
	}

	r.log.Infof("request finished: %d events published", n)

	return nil
}

// getIdsFromResponses returns ids from responses
func (r *requester) getIdsFromResponses(intermediateResps []*http.Response, replace *regexp.Regexp) ([]string, error) {
	var b []byte
	var ids []string
	var err error
	// collect ids from all responses
	for _, resp := range intermediateResps {
		resp := resp
		if resp.Body != nil {
			defer resp.Body.Close()
			b, err = io.ReadAll(resp.Body)
			if err != nil {
				return nil, fmt.Errorf("error while reading response body: %w", err)
			}
		}

		// get replace values from collected json
		ids, err = parse(string(b), replace.String())
		if err != nil {
			return nil, fmt.Errorf("error while getting keys: %w", err)
		}
	}
	return ids, nil
}

// processAndPublishEvents process and publish events based on response type
func (r *requester) processAndPublishEvents(stdCtx context.Context, trCtx *transformContext, publisher inputcursor.Publisher, finalResps []*http.Response, publish bool) (int, error) {
	eventsCh, err := r.responseProcessor.startProcessing(stdCtx, trCtx, finalResps)
	if err != nil {
		return 0, fmt.Errorf("error starting response processor: %w", err)
	}

	trCtx.clearIntervalData()

	var n int
	for maybeMsg := range eventsCh {
		if maybeMsg.failed() {
			r.log.Errorf("error processing response: %v", maybeMsg)
			continue
		}

		if publish {
			event, err := makeEvent(maybeMsg.msg)
			if err != nil {
				r.log.Errorf("error creating event: %v", maybeMsg)
				continue
			}

			if err := publisher.Publish(event, trCtx.cursorMap()); err != nil {
				r.log.Errorf("error publishing event: %v", err)
				continue
			}
		}
		if len(*trCtx.firstEventClone()) == 0 {
			trCtx.updateFirstEvent(maybeMsg.msg)
		}
		trCtx.updateLastEvent(maybeMsg.msg)
		trCtx.updateCursor()
		n++
	}
	return n, nil
}
