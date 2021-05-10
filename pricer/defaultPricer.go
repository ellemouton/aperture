package pricer

import "context"

type defaultPricer struct {
	Price int64
}

// NewDefaultPricer initialises a new defaultPrice provider where each resource
// for the service will have the same price.
func NewDefaultPricer(price int64) *defaultPricer {
	return &defaultPricer{Price: price}
}

// GetPrice returns the price charged for all resources of a service.
// It is part of the Pricer interface.
func (d *defaultPricer) GetPrice(_ context.Context, _ string) (int64,
	error) {

	return d.Price, nil
}

// Close is part of the Pricer interface. For the defaultPricer, the method does
// nothing.
func (d *defaultPricer) Close() error {
	return nil
}
