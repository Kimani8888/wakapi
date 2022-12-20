package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/emvi/logbuch"
	"github.com/gorilla/mux"
	conf "github.com/muety/wakapi/config"
	"github.com/muety/wakapi/middlewares"
	"github.com/muety/wakapi/services"
	"github.com/stripe/stripe-go/v74"
	stripePortalSession "github.com/stripe/stripe-go/v74/billingportal/session"
	stripeCheckoutSession "github.com/stripe/stripe-go/v74/checkout/session"
	stripeCustomer "github.com/stripe/stripe-go/v74/customer"
	stripePrice "github.com/stripe/stripe-go/v74/price"
	"github.com/stripe/stripe-go/v74/webhook"
	"io/ioutil"
	"net/http"
	"time"
)

type SubscriptionHandler struct {
	config     *conf.Config
	userSrvc   services.IUserService
	mailSrvc   services.IMailService
	httpClient *http.Client
}

func NewSubscriptionHandler(
	userService services.IUserService,
	mailService services.IMailService,
) *SubscriptionHandler {
	config := conf.Get()

	if config.Subscriptions.Enabled {
		stripe.Key = config.Subscriptions.StripeSecretKey

		price, err := stripePrice.Get(config.Subscriptions.StandardPriceId, nil)
		if err != nil {
			logbuch.Fatal("failed to fetch stripe plan details: %v", err)
		}
		config.Subscriptions.StandardPrice = fmt.Sprintf("%2.f €", price.UnitAmountDecimal/100.0) // TODO: respect actual currency

		logbuch.Info("enabling subscriptions with stripe payment for %s / month", config.Subscriptions.StandardPrice)
	}

	return &SubscriptionHandler{
		config:     config,
		userSrvc:   userService,
		mailSrvc:   mailService,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// https://stripe.com/docs/billing/quickstart?lang=go

func (h *SubscriptionHandler) RegisterRoutes(router *mux.Router) {
	if !h.config.Subscriptions.Enabled {
		return
	}

	subRouterPublic := router.PathPrefix("/subscription").Subrouter()
	subRouterPublic.Path("/success").Methods(http.MethodGet).HandlerFunc(h.GetCheckoutSuccess)
	subRouterPublic.Path("/cancel").Methods(http.MethodGet).HandlerFunc(h.GetCheckoutCancel)
	subRouterPublic.Path("/webhook").Methods(http.MethodPost).HandlerFunc(h.PostWebhook)

	subRouterPrivate := subRouterPublic.PathPrefix("").Subrouter()
	subRouterPrivate.Use(
		middlewares.NewAuthenticateMiddleware(h.userSrvc).WithRedirectTarget(defaultErrorRedirectTarget()).Handler,
	)
	subRouterPrivate.Path("/checkout").Methods(http.MethodPost).HandlerFunc(h.PostCheckout)
	subRouterPrivate.Path("/portal").Methods(http.MethodPost).HandlerFunc(h.PostPortal)
}

func (h *SubscriptionHandler) PostCheckout(w http.ResponseWriter, r *http.Request) {
	if h.config.IsDev() {
		loadTemplates()
	}

	user := middlewares.GetPrincipal(r)
	if user.Email == "" {
		http.Redirect(w, r, fmt.Sprintf("%s/settings?error=%s#subscription", h.config.Server.BasePath, "missing e-mail address"), http.StatusFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, fmt.Sprintf("%s/settings?error=%s#subscription", h.config.Server.BasePath, "missing form values"), http.StatusFound)
		return
	}

	checkoutParams := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    &h.config.Subscriptions.StandardPriceId,
				Quantity: stripe.Int64(1),
			},
		},
		CustomerEmail:     &user.Email,
		ClientReferenceID: &user.Email,
		SuccessURL:        stripe.String(fmt.Sprintf("%s%s/subscription/success", h.config.Server.PublicUrl, h.config.Server.BasePath)),
		CancelURL:         stripe.String(fmt.Sprintf("%s%s/subscription/cancel", h.config.Server.PublicUrl, h.config.Server.BasePath)),
	}

	session, err := stripeCheckoutSession.New(checkoutParams)
	if err != nil {
		conf.Log().Request(r).Error("failed to create stripe checkout session: %v", err)
		http.Redirect(w, r, fmt.Sprintf("%s/settings?error=%s#subscription", h.config.Server.BasePath, "something went wrong"), http.StatusFound)
		return
	}

	http.Redirect(w, r, session.URL, http.StatusSeeOther)
}

func (h *SubscriptionHandler) PostPortal(w http.ResponseWriter, r *http.Request) {
	if h.config.IsDev() {
		loadTemplates()
	}

	user := middlewares.GetPrincipal(r)
	if user.Email == "" {
		http.Redirect(w, r, fmt.Sprintf("%s/settings?error=%s#subscription", h.config.Server.BasePath, "no subscription found with your e-mail address, please contact us!"), http.StatusFound)
		return
	}

	customer, err := h.findStripeCustomerByEmail(user.Email)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("%s/settings?error=%s#subscription", h.config.Server.BasePath, "no subscription found with your e-mail address, please contact us!"), http.StatusFound)
		return
	}

	portalParams := &stripe.BillingPortalSessionParams{
		Customer:  &customer.ID,
		ReturnURL: &h.config.Server.PublicUrl,
	}

	session, err := stripePortalSession.New(portalParams)
	if err != nil {
		conf.Log().Request(r).Error("failed to create stripe portal session: %v", err)
		http.Redirect(w, r, fmt.Sprintf("%s/settings?error=%s#subscription", h.config.Server.BasePath, "something went wrong"), http.StatusFound)
		return
	}

	http.Redirect(w, r, session.URL, http.StatusSeeOther)
}

func (h *SubscriptionHandler) PostWebhook(w http.ResponseWriter, r *http.Request) {
	bodyReader := http.MaxBytesReader(w, r.Body, int64(65536))
	payload, err := ioutil.ReadAll(bodyReader)
	if err != nil {
		conf.Log().Request(r).Error("error in stripe webhook request: %v", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	event, err := webhook.ConstructEventWithOptions(payload, r.Header.Get("Stripe-Signature"), h.config.Subscriptions.StripeEndpointSecret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		conf.Log().Request(r).Error("stripe webhook signature verification failed: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	switch event.Type {
	case "customer.subscription.deleted",
		"customer.subscription.updated",
		"customer.subscription.created":
		subscription, customer, err := h.handleParseSubscription(w, r, event)
		if err != nil {
			return
		}
		logbuch.Info("received stripe subscription event of type '%s' for subscription '%d' (customer '%s' with email '%s').", event.Type, subscription.ID, customer.ID, customer.Email)
	// TODO: handle
	// if status == 'active', set active subscription date to current_period_end
	// if status == 'canceled' or 'unpaid', clear active subscription date, if < now
	// example payload: https://pastr.de/p/k7bx3alx38b1iawo6amtx09k
	default:
		logbuch.Warn("got stripe event '%s' with no handler defined", event.Type)
	}

	w.WriteHeader(http.StatusOK)
}

func (h *SubscriptionHandler) GetCheckoutSuccess(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, fmt.Sprintf("%s/settings?success=%s#subscription", h.config.Server.BasePath, "you have successfully subscribed to Wakapi!"), http.StatusFound)
}

func (h *SubscriptionHandler) GetCheckoutCancel(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, fmt.Sprintf("%s/settings#subscription", h.config.Server.BasePath), http.StatusFound)
}

func (h *SubscriptionHandler) handleParseSubscription(w http.ResponseWriter, r *http.Request, event stripe.Event) (*stripe.Subscription, *stripe.Customer, error) {
	var subscription stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &subscription); err != nil {
		conf.Log().Request(r).Error("failed to parse stripe webhook payload: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return nil, nil, err
	}

	customer, err := stripeCustomer.Get(subscription.Customer.ID, nil)
	if err != nil {
		conf.Log().Request(r).Error("failed to fetch stripe customer (%s): %v", subscription.Customer.ID, err)
		w.WriteHeader(http.StatusBadRequest)
		return nil, nil, err
	}

	logbuch.Info("associated stripe customer %s with user %s", customer.ID, customer.Email)

	return &subscription, customer, nil
}

func (h *SubscriptionHandler) findStripeCustomerByEmail(email string) (*stripe.Customer, error) {
	params := &stripe.CustomerSearchParams{
		SearchParams: stripe.SearchParams{
			Query: fmt.Sprintf(`email:"%s"`, email),
		},
	}

	results := stripeCustomer.Search(params)
	if err := results.Err(); err != nil {
		return nil, err
	}

	if results.Next() {
		return results.Customer(), nil
	} else {
		return nil, errors.New("no customer found with given criteria")
	}
}