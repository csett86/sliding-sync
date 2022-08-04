package sync3

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/matrix-org/sync-v3/internal"
)

type ConnID struct {
	DeviceID string
}

func (c *ConnID) String() string {
	return c.DeviceID
}

type ConnHandler interface {
	// Callback which is allowed to block as long as the context is active. Return the response
	// to send back or an error. Errors of type *internal.HandlerError are inspected for the correct
	// status code to send back.
	OnIncomingRequest(ctx context.Context, cid ConnID, req *Request, isInitial bool) (*Response, error)
	UserID() string
	Destroy()
	Alive() bool
}

// Conn is an abstraction of a long-poll connection. It automatically handles the position values
// of the /sync request, including sending cached data in the event of retries. It does not handle
// the contents of the data at all.
type Conn struct {
	ConnID ConnID

	handler ConnHandler

	// The position/data in the stream last sent by the client
	lastClientRequest Request

	// A buffer of the last responses sent to the client.
	// Can be resent as-is if the server response was lost.
	// We always send back [0]
	serverResponses []Response
	lastPos         int64

	// ensure only 1 incoming request is handled per connection
	mu                       *sync.Mutex
	cancelOutstandingRequest func()
}

func NewConn(connID ConnID, h ConnHandler) *Conn {
	return &Conn{
		ConnID:  connID,
		handler: h,
		mu:      &sync.Mutex{},
	}
}

func (c *Conn) Alive() bool {
	return c.handler.Alive()
}

func (c *Conn) tryRequest(ctx context.Context, req *Request) (res *Response, err error) {
	defer func() {
		panicErr := recover()
		if panicErr != nil {
			err = fmt.Errorf("panic: %s", panicErr)
			logger.Error().Msg(string(debug.Stack()))
		}
	}()
	return c.handler.OnIncomingRequest(ctx, c.ConnID, req, req.pos == 0)
}

// OnIncomingRequest advances the clients position in the stream, returning the response position and data.
func (c *Conn) OnIncomingRequest(ctx context.Context, req *Request) (resp *Response, herr *internal.HandlerError) {
	if c.cancelOutstandingRequest != nil {
		c.cancelOutstandingRequest()
	}
	c.mu.Lock()
	ctx, cancel := context.WithCancel(ctx)
	c.cancelOutstandingRequest = cancel
	// it's intentional for the lock to be held whilst inside HandleIncomingRequest
	// as it guarantees linearisation of data within a single connection
	defer c.mu.Unlock()

	isFirstRequest := req.pos == 0
	isRetransmit := !isFirstRequest && c.lastClientRequest.pos == req.pos
	acksOldestResponse := len(c.serverResponses) > 0 && req.pos == c.serverResponses[0].PosInt()
	acksLatestResponse := c.lastPos != 0 && c.lastPos == req.pos
	isSameRequest := !isFirstRequest && c.lastClientRequest.Same(req)

	// if there is a position and it isn't something we've told the client nor a retransmit, they
	// are playing games
	if !isFirstRequest && !acksLatestResponse && !acksOldestResponse && !isRetransmit {
		// the client made up a position, reject them
		logger.Trace().Int64("pos", req.pos).Msg("unknown pos")
		return nil, &internal.HandlerError{
			StatusCode: 400,
			Err:        fmt.Errorf("unknown position: %d", req.pos),
		}
	}

	// purge the response buffer based on the client's new position. Higher pos values are later.
	delIndex := -1
	for i := range c.serverResponses {
		if req.pos >= c.serverResponses[i].PosInt() {
			// the client has sent this pos ergo they have seen this response before, so forget it.
			delIndex = i
		} else {
			break
		}
	}
	c.serverResponses = c.serverResponses[delIndex+1:] // slice out the first delIndex+1 elements

	defer func() {
		l := logger.Trace().Int("num_res_acks", delIndex+1).Bool("is_retransmit", isRetransmit).Bool("acks_oldest", acksOldestResponse).Bool(
			"acks_newest", acksLatestResponse,
		).Bool("is_first", isFirstRequest).Bool("is_same", isSameRequest).Int64("pos", req.pos).Str("user", c.handler.UserID())
		if len(c.serverResponses) > 0 {
			l.Int64("new_pos", c.serverResponses[0].PosInt())
		}

		l.Msg("OnIncomingRequest finished")
	}()

	if !isFirstRequest {
		if isRetransmit {
			// if the request bodies match up then this is a retry, else it could be the client modifying
			// their filter params, so fallthrough
			if isSameRequest {
				// this is the 2nd+ time we've seen this request, meaning the client likely retried this
				// request. Send the response we sent before.
				logger.Trace().Int64("pos", req.pos).Msg("returning cached response for pos")
				return &c.serverResponses[0], nil
			} else {
				logger.Info().Int64("pos", req.pos).Msg("client has resent this pos with different request data")
				// we need to fallthrough to process this request as the client will not resend this request data,
			}
		}
	}

	// if the client has no new data for us but we still have buffered responses, return that rather than
	// invoking the handler.
	if len(c.serverResponses) > 0 {
		if isSameRequest {
			return &c.serverResponses[0], nil
		}
		// we have buffered responses but we cannot return it else we'll ignore the data in this request,
		// so we need to wait for this incoming request to be processed _before_ we can return the data.
		// To ensure this doesn't take too long, be cheeky and inject a low timeout value to ensure we
		// don't needlessly block.
		req.SetTimeoutMSecs(1)
	}

	resp, err := c.tryRequest(ctx, req)
	if err != nil {
		herr, ok := err.(*internal.HandlerError)
		if !ok {
			herr = &internal.HandlerError{
				StatusCode: 500,
				Err:        err,
			}
		}
		return nil, herr
	}
	// assign the last client request now _after_ we have processed the request so we don't incorrectly
	// cache errors or panics and result in getting wedged or tightlooping.
	c.lastClientRequest = *req
	// this position is the highest stored pos +1
	resp.Pos = fmt.Sprintf("%d", c.lastPos+1)
	resp.TxnID = req.TxnID
	// buffer it
	c.serverResponses = append(c.serverResponses, *resp)
	c.lastPos = resp.PosInt()

	// return the oldest value
	return &c.serverResponses[0], nil
}
