package pricer

import (
	"context"
	"fmt"

	"github.com/lightninglabs/aperture/pricesrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type Config struct {
	// GRPCAddress is the address that the pricer gRPC server is serving on.
	GRPCAddress string `long:"grpcaddress" description:"gRPC addr to use for price info for service resources"`

	// Insecure indicates if the connection to the gRPC server should use
	// TLS encryption or not.
	Insecure bool `long:"insecure" description: Set to true if no TLS encryption is to be used`

	// TLSCertPath is the path the the tls cert used by the price server.
	TLSCertPath string `long:"tlscertpath" description:"Path to the servers tls cert"`
}

type gRPCPricer struct {
	rpcConn   *grpc.ClientConn
	rpcClient pricesrpc.PricesClient
}

// NewGRPCPricer initialises a Pricer backed by a gRPC backend server.
func NewGRPCPricer(cfg Config) (*gRPCPricer, error) {
	var (
		c   gRPCPricer
		err error
	)

	var opt grpc.DialOption

	if cfg.Insecure {
		opt = grpc.WithInsecure()
	} else {
		tlsCredentials, err := credentials.NewClientTLSFromFile(
			cfg.TLSCertPath, "",
		)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to load TLS cert %s: %v",
				cfg.TLSCertPath, err,
			)
		}
		opt = grpc.WithTransportCredentials(tlsCredentials)
	}

	c.rpcConn, err = grpc.Dial(cfg.GRPCAddress, opt)
	if err != nil {
		return nil, err
	}

	c.rpcClient = pricesrpc.NewPricesClient(c.rpcConn)

	return &c, nil
}

// GetPrice queries the server for the price of a resource path and returns the
// price. GetPrice is part of the Pricer interface.
func (c gRPCPricer) GetPrice(ctx context.Context, path string) (int64, error) {
	resp, err := c.rpcClient.GetPrice(ctx, &pricesrpc.GetPriceRequest{
		Path: path,
	})
	if err != nil {
		return 0, err
	}

	return resp.Price, nil
}

// Close closes the gRPC connection. It is part of the Pricer interface.
func (c gRPCPricer) Close() error {
	return c.rpcConn.Close()
}
