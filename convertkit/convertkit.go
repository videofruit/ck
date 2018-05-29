// Package convertkit provides a client to the ConvertKit API v3.
// See http://help.convertkit.com/article/33-api-documentation-v3
package convertkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

var (
	// ErrKeyMissing is returned when the API key is required, but not present.
	ErrKeyMissing = errors.New("ConvertKit API key missing")

	// ErrSecretMissing is returned when the API secret is required, but not present.
	ErrSecretMissing = errors.New("ConvertKit API secret missing")
)

// Config is used to configure the creation of the client.
type Config struct {
	Endpoint string
	Key      string
	Secret   string

	ConcurrentRequests int

	HTTPClient *http.Client
}

// DefaultConfig returns a default configuration for the client. It parses the
// environment variables CONVERTKIT_ENDPOINT, CONVERTKIT_API_KEY, and
// CONVERTKIT_API_SECRET.
func DefaultConfig() *Config {
	c := Config{
		Endpoint:           "https://api.convertkit.com",
		ConcurrentRequests: 8,
		HTTPClient:         http.DefaultClient,
	}
	if v := os.Getenv("CONVERTKIT_API_ENDPOINT"); v != "" {
		c.Endpoint = v
	}
	if v := os.Getenv("CONVERTKIT_API_KEY"); v != "" {
		c.Key = v
	}
	if v := os.Getenv("CONVERTKIT_API_SECRET"); v != "" {
		c.Secret = v
	}
	return &c
}

// Client is the client to the ConvertKit API. Create a client with NewClient.
type Client struct {
	config *Config
}

// NewClient returns a new client for the given configuration.
func NewClient(c *Config) (*Client, error) {
	defConfig := DefaultConfig()
	if c.Endpoint == "" {
		c.Endpoint = defConfig.Endpoint
	}
	if c.Key == "" {
		c.Key = defConfig.Key
	}
	if c.Secret == "" {
		c.Secret = defConfig.Secret
	}
	if c.ConcurrentRequests == 0 {
		c.ConcurrentRequests = defConfig.ConcurrentRequests
	}
	if c.HTTPClient == nil {
		c.HTTPClient = defConfig.HTTPClient
	}
	return &Client{config: c}, nil
}

// SubscriberQuery parameterizes what subscriber data to request.
type SubscriberQuery struct {
	Since, Until string // TODO: turn this into time.Date
	Reverse      bool
	Cancelled    bool
	EmailAddress string
}

// Subscriber describes a ConvertKit subscriber.
type Subscriber struct {
	ID           int               `json:"id"`
	FirstName    string            `json:"first_name"`
	EmailAddress string            `json:"email_address"`
	State        string            `json:"state"`
	CreatedAt    time.Time         `json:"created_at"`
	Fields       map[string]string `json:"fields"`
}

type subscriberPage struct {
	TotalSubscribers int          `json:"total_subscribers"`
	Page             int          `json:"page"`
	TotalPages       int          `json:"total_pages"`
	Subscribers      []Subscriber `json:"subscribers"`
}

// Subscription is a subscriber's association with a tag
type Subscription struct {
	ID               int        `json:"id"`
	State            string     `json:"state"`
	CreatedAt        time.Time  `json:"created_at"`
	Source           string     `json:"source"`
	Referrer         string     `json:"referrer"`
	SubscribableID   int        `json:"subscribable_id"`
	SubscribableType string     `json:"subscribable_type"`
	Subscriber       Subscriber `json:"subscriber"`
}

// SubscriptionRequest is a set of parameters to subscriber a subscriber to a tag
//
// `ApiKey` will use the client's ApiKey if unspecified
// `Email` and at least one `Tags` are required.
// `Fields` keys must be custom field names already specified in ConvertKit.
// This API call cannot create new custom fields.
type SubscriptionRequest struct {
	Email     string            `json:"email"`
	FirstName string            `json:"first_name"`
	Fields    map[string]string `json:"fields"`
	Tags      []int             `json:"tags"`
	APIKey    string            `json:"api_key"`
}

// AddTag appends newTag to Tags if it is not alredy in Tags
func (r *SubscriptionRequest) AddTag(newTag int) {
	for _, tag := range r.Tags {
		if tag == newTag {
			return
		}
	}
	r.Tags = append(r.Tags, newTag)
}

// Subscribers returns a list of all confirmed subscribers.
func (c *Client) Subscribers(query *SubscriberQuery) ([]Subscriber, error) {
	p, err := c.subscriberPage(1, query)
	if err != nil {
		return nil, err
	}

	total := p.TotalPages
	if total <= 1 {
		return p.Subscribers, nil
	}

	var g errgroup.Group
	limiter := make(chan bool, c.config.ConcurrentRequests)

	pages := make([]subscriberPage, total)
	pages[0] = *p

	for i := 2; i <= total; i++ {
		i := i // see https://golang.org/doc/faq#closures_and_goroutines
		g.Go(func() error {
			limiter <- true
			defer func() { <-limiter }()

			p, err := c.subscriberPage(i, query)
			if err == nil {
				pages[i-1] = *p
			}
			return err
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	var subscribers []Subscriber
	for i := 0; i < total; i++ {
		subscribers = append(subscribers, pages[i].Subscribers...)
	}

	return subscribers, nil
}

// TotalSubscribers returns the number of confirmed subscribers.
func (c *Client) TotalSubscribers() (int, error) {
	p, err := c.subscriberPage(1, nil)
	if err != nil {
		return 0, err
	}
	return p.TotalSubscribers, nil
}

// TagSubscriber adds a tag to a subscriber
//
// This method will also create a subscriber with the email address provided if one does not exist.
func (c *Client) TagSubscriber(email string, tagID int) (Subscription, error) {

	return c.CreateTagSubscription(SubscriptionRequest{
		Email: email,
		Tags:  []int{tagID},
	})
}

// CreateTagSubscription tags a subscriber with a tag whild allowing access to set the optional parameters
// allowed by ConvertKit through a `SubscriptionRequest`
func (c *Client) CreateTagSubscription(req SubscriptionRequest) (Subscription, error) {
	subscription := Subscription{}
	if len(req.Tags) < 1 {
		return subscription, errors.New("Must specify at least one Tag to create a subscription")
	}

	if req.APIKey == "" {
		req.APIKey = c.config.Key
	}

	path := fmt.Sprintf("/v3/tags/%d/subscribe", req.Tags[0])
	response := struct {
		Subscription Subscription `json:"subscription"`
	}{}
	err := c.postJSON(path, req, &response)
	return response.Subscription, err
}

func (c *Client) subscriberPage(page int, query *SubscriberQuery) (*subscriberPage, error) {
	if c.config.Secret == "" {
		return nil, ErrSecretMissing
	}

	url := fmt.Sprintf("%s/v3/subscribers?api_secret=%s&page=%d",
		c.config.Endpoint, c.config.Secret, page)

	if query != nil {
		if query.Since != "" {
			since, err := parseDate(query.Since)
			if err != nil {
				return nil, err
			}
			url += fmt.Sprintf("&from=%s", since)
		}
		if query.Until != "" {
			until, err := parseDate(query.Until)
			if err != nil {
				return nil, err
			}
			url += fmt.Sprintf("&to=%s", until)
		}
		if query.Reverse {
			url += "&sort_order=desc"
		}
		if query.Cancelled {
			url += "&sort_field=cancelled_at"
		}
		if query.EmailAddress != "" {
			url += fmt.Sprintf("&email_address=%s", query.EmailAddress)
		}
	}

	var p subscriberPage
	if err := c.sendRequest("GET", url, nil, &p); err != nil {
		return nil, err
	}

	return &p, nil
}

func parseDate(date string) (string, error) {
	const format string = "2006-01-02"
	if strings.ToLower(date) == "yesterday" {
		return time.Now().Add(-24 * time.Hour).Format(format), nil
	}
	if _, err := time.Parse(format, date); err != nil {
		return "", err
	}
	return date, nil
}

func (c *Client) sendRequest(method, url string, body io.Reader, out interface{}) error {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}

	resp, err := c.config.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP error: %s", resp.Status)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) postJSON(path string, reqStruct, out interface{}) error {
	body, err := json.Marshal(reqStruct)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", c.config.Endpoint+path, bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.config.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP error: %s", resp.Status)
	}

	return json.NewDecoder(resp.Body).Decode(out)

}
