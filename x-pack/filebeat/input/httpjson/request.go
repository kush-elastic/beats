// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"

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
		body, _ := ioutil.ReadAll(resp.Body)
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
	replace    string
}

func newRequestFactory(config *requestConfig, configChain []chainsConfig, authConfig *authConfig, log *logp.Logger) []*requestFactory {
	// config validation already checked for errors here
	var rf []*requestFactory
	ts, _ := newBasicTransformsFromConfig(config.Transforms, requestNamespace, log)
	// regular call requestFactory object
	rfs := &requestFactory{
		url:        *config.URL.URL,
		method:     config.Method,
		body:       config.Body,
		transforms: ts,
		log:        log,
		encoder:    registeredEncoders[config.EncodeAs],
	}
	if authConfig != nil && authConfig.Basic.isEnabled() {
		rfs.user = authConfig.Basic.User
		rfs.password = authConfig.Basic.Password
	}
	rf = append(rf, rfs)
	for _, ch := range configChain {
		// chain calls requestFactory object
		rfs := &requestFactory{
			url:        *ch.Step.Request.URL.URL,
			method:     ch.Step.Request.Method,
			body:       ch.Step.Request.Body,
			transforms: ts,
			log:        log,
			encoder:    registeredEncoders[config.EncodeAs],
			replace:    ch.Step.Replace,
		}
		rf = append(rf, rfs)
	}
	return rf
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
	requestFactory    []*requestFactory
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
		requestFactory:    requestFactory,
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

// generateNewUrl returns new url value using replacement from oldUrl with str ids
func generateNewUrl(replacement string, oldUrl string, str []byte) (url.URL, error) {
	reg, err := regexp.Compile(replacement)
	if err != nil {
		return url.URL{}, fmt.Errorf("failed to create regex on provided value: %w", err)
	}
	newUrl, err := url.Parse(string(reg.ReplaceAll([]byte(oldUrl), str)))
	if err != nil {
		return url.URL{}, fmt.Errorf("failed to replace value in url: %w", err)
	}
	return *newUrl, nil

}

func (r *requester) doRequest(stdCtx context.Context, trCtx *transformContext, publisher inputcursor.Publisher) error {
	var (
		httpResp       *http.Response
		ArrayhttpResp  []*http.Response
		finalHttpResps []*http.Response
		st             []string
		err            error
		b              []byte
	)
	// perform each requestFactory
	for i, rf := range r.requestFactory {
		// iterate over collected ids from last response
		if i != 0 {
			// perform request over collected ids
			for _, s := range st {
				// reformat urls of requestFactory using ids
				rf.url, err = generateNewUrl(rf.replace, rf.url.String(), []byte(s))
				if err != nil {
					return fmt.Errorf("failed to generate new URL: %w", err)
				}

				// collect data from new urls
				httpResp, err = rf.collectResponse(stdCtx, trCtx, r, publisher)
				if err != nil {
					return fmt.Errorf("failed to execute http client.Do: %w", err)
				}
				// store data according to response type
				if i+1 == len(r.requestFactory) && len(st) != 0 {
					finalHttpResps = append(finalHttpResps, httpResp)
				} else {
					ArrayhttpResp = append(ArrayhttpResp, httpResp)
				}
			}
		} else {
			// perform and store regular call responses
			httpResp, err = rf.collectResponse(stdCtx, trCtx, r, publisher)
			if err != nil {
				return fmt.Errorf("failed to execute http client.Do: %w", err)
			}
			if len(r.requestFactory) <= 1 {
				finalHttpResps = append(finalHttpResps, httpResp)
			} else {
				ArrayhttpResp = append(ArrayhttpResp, httpResp)
			}
		}

		// parse and store ids from collected responses
		if i+1 < len(r.requestFactory) {
			st = nil
			// collect ids from all responses
			for _, resp := range ArrayhttpResp {
				if resp.Body != nil {
					b, err = ioutil.ReadAll(resp.Body)
					if err != nil {
						return err
					}
				}
				// Restore the io.ReadCloser to its original state
				resp.Body = ioutil.NopCloser(bytes.NewBuffer(b))
				// get replace values from collected json
				st, err = getJSON(string(b), r.requestFactory[i+1].replace)
				if err != nil {
					return err
				}
			}
		}
	}

	eventsCh, err := r.responseProcessor.startProcessing(stdCtx, trCtx, finalHttpResps)
	if err != nil {
		return err
	}

	trCtx.clearIntervalData()

	var n int
	for maybeMsg := range eventsCh {
		if maybeMsg.failed() {
			r.log.Errorf("error processing response: %v", maybeMsg)
			continue
		}

		event, err := makeEvent(maybeMsg.msg)
		if err != nil {
			r.log.Errorf("error creating event: %v", maybeMsg)
			continue
		}

		if err := publisher.Publish(event, trCtx.cursorMap()); err != nil {
			r.log.Errorf("error publishing event: %v", err)
			continue
		}
		if len(*trCtx.firstEventClone()) == 0 {
			trCtx.updateFirstEvent(maybeMsg.msg)
		}
		trCtx.updateLastEvent(maybeMsg.msg)
		trCtx.updateCursor()
		n++
	}

	httpResp.Body.Close()
	r.log.Infof("request finished: %d events published", n)

	return nil
}

// getJSON returns array of string values from json string
func getJSON(b, str string) ([]string, error) {
	strArr := strings.Split(str, ".")
	bNew := []byte(b)
	newIn, err := jsonInterface(string(b[0]), strArr[0], bNew)
	if err != nil {
		return nil, fmt.Errorf("error while parsing json: %v", err)
	}
	var strArray []string
	for i, sr := range strArr {
		switch sr {
		case "#":
			newIn, err = jsonArr(strArr[i+1], newIn)
			if err != nil {
				return nil, fmt.Errorf("error while parsing json: %v", err)
			}
			for _, ir := range newIn.([]interface{}) {
				if reflect.TypeOf(ir).String() == "string" {
					strArray = append(strArray, ir.(string))
				} else {
					final, err := jsonNorm(strArr[i+2], ir)
					if err != nil {
						return nil, fmt.Errorf("error while parsing json: %v", err)
					}
					strArray = append(strArray, final.(string))
				}
			}
		default:
			newIn, err = jsonNorm(sr, newIn)
			if err != nil {
				return nil, fmt.Errorf("error while parsing json: %v", err)
			}
			if len(strArr) == i+1 {
				strArray = append(strArray, newIn.(string))
			}
		}
		if sr == "#" {
			break
		}
	}
	return strArray, nil
}

// jsonArr returns array of interface value from interface
func jsonArr(sr string, bNew interface{}) ([]interface{}, error) {
	var str []interface{}
	if bNew.([]interface{}) != nil {
		for _, i := range bNew.([]interface{}) {
			str = append(str, i.(map[string]interface{})[sr])
		}
	} else {
		return nil, fmt.Errorf("error while parsing json")
	}

	return str, nil
}

// jsonInterface returns interface from byte json
func jsonInterface(str, comStr string, bNew []byte) (interface{}, error) {
	var data map[string]interface{}
	if str == "{" && comStr != "#" {
		err := json.Unmarshal(bNew, &data)
		if err != nil {
			return nil, err
		}
	} else if str == "[" && comStr == "#" {
		var data1 []interface{}
		err := json.Unmarshal(bNew, &data1)
		if err != nil {
			return nil, fmt.Errorf("error parsing Array of json: ")
		}
		return data1, nil
	} else if str == comStr && comStr != "#" {
		err := json.Unmarshal(bNew, &data)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("error parsing json: ")
	}
	return data, nil
}

// jsonNorm returns interface value from interface
func jsonNorm(sr string, bNew interface{}) (interface{}, error) {
	if reflect.TypeOf(bNew).String() != "map[string]interface {}" {
		return nil, fmt.Errorf("error while parsing json")
	}
	return bNew.(map[string]interface{})[sr], nil
}
