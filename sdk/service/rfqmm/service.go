package rfqmm

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	ethutils "github.com/celer-network/goutils/eth"
	"github.com/celer-network/goutils/log"
	"github.com/celer-network/peti-rfq-mm/sdk/bindings/rfq"
	"github.com/celer-network/peti-rfq-mm/sdk/common"
	"github.com/celer-network/peti-rfq-mm/sdk/eth"
	rfqserver "github.com/celer-network/peti-rfq-mm/sdk/service/rfq"
	rfqproto "github.com/celer-network/peti-rfq-mm/sdk/service/rfq/proto"
	"github.com/celer-network/peti-rfq-mm/sdk/service/rfqmm/proto"
	"google.golang.org/grpc"
)

const (
	DefaultReportRetryPeriod int64 = 5
	DefaultProcessPeriod     int64 = 5
	DefaultPriceValidPeriod  int64 = 300
	DefaultDstTransferPeriod int64 = 3000
	DefaultPortListenOn      int64 = 5555
)

type Client struct {
	proto.ApiClient
	server string
	conn   *grpc.ClientConn
}

type Server struct {
	Ctl               chan bool
	RfqClient         *rfqserver.Client
	Config            *ServerConfig
	ChainCaller       ChainQuerier
	LiquidityProvider LiquidityProvider
	AmountCalculator  AmountCalculator
	RequestSigner     RequestSigner
}

type Update struct {
	Hash   eth.Hash
	Status rfqproto.OrderStatus
}

type ServerConfig struct {
	// the period for retrying report supported tokens to rfq server
	ReportRetryPeriod int64
	// the period for processing pending orders
	ProcessPeriod int64
	// indicates the period for a price to be valid
	PriceValidPeriod int64
	// minimum dst transfer period, in order to give mm enough time for dst transfer
	DstTransferPeriod int64
	// token pair policy list
	TPPolicyList []string
	// port num that mm would listen on
	PortListenOn int64
	// light mm, which needs a relayer to interact with rfq server
	LightMM bool
	// if not set, will use localhost
	Host string
}

func (config *ServerConfig) clean() {
	if config.ReportRetryPeriod == 0 {
		config.ReportRetryPeriod = DefaultReportRetryPeriod
		log.Debugf("Got 0 ReportRetryPeriod, use default value(%d) instead.", DefaultReportRetryPeriod)
	}
	if config.ProcessPeriod == 0 {
		config.ProcessPeriod = DefaultProcessPeriod
		log.Debugf("Got 0 ProcessPeriod, use default value(%d) instead.", DefaultProcessPeriod)
	}
	if config.PriceValidPeriod == 0 {
		config.PriceValidPeriod = DefaultPriceValidPeriod
		log.Debugf("Got 0 PriceValidPeriod, use default value(%d) instead.", DefaultPriceValidPeriod)
	}
	if config.DstTransferPeriod == 0 {
		config.DstTransferPeriod = DefaultDstTransferPeriod
		log.Debugf("Got 0 DstTransferPeriod, use default value(%d) instead.", DefaultDstTransferPeriod)
	}
	if len(config.TPPolicyList) == 0 {
		log.Debugf("No token pair policy was given.")
	}
	if config.PortListenOn == 0 {
		config.PortListenOn = DefaultPortListenOn
		log.Debugf("Got 0 PortListenOn, use default value(%d) instead.", DefaultPortListenOn)
	}
}

type ChainQuerier interface {
	GetRfqContract(chainId uint64) (eth.Addr, error)
	GetRfqFee(srcChainId, dstChainId uint64, amount *big.Int) (*big.Int, error)
	GetMsgFee(chainId uint64) (*big.Int, error)
	GetGasPrice(chainId uint64) (*big.Int, error)
	GetNativeWrap(chainId uint64) (*common.Token, error)
	GetERC20Balance(chainId uint64, token, account eth.Addr) (*big.Int, error)
	GetNativeBalance(chainId uint64, accoun eth.Addr) (*big.Int, error)
	GetQuoteStatus(chainId uint64, quoteHash eth.Hash) (uint8, error)
	VerifyRfqEvent(chainId uint64, tx eth.Hash, evName string) (bool, error)
}

type LiquidityProvider interface {
	// IsPaused returns whether the LiquidityProvider is paused or not
	IsPaused() bool
	// GetTokens returns a list of all supported tokens
	GetTokens() []*common.Token
	// SetupTokenPairs sets up supported token pairs based on a given policy list.
	SetupTokenPairs(policies []string)
	// HasTokenPair check if a given token pair is supported
	HasTokenPair(srcToken, dstToken *common.Token) bool
	// GetLiquidityProviderAddr returns the address of liquidity provider on specified chain
	GetLiquidityProviderAddr(chainId uint64) (eth.Addr, error)
	// AskForFreezing checks if there is sufficient liquidity for specified token on specified chain and returns freeze time
	AskForFreezing(chainId uint64, token eth.Addr, amount *big.Int, isNative bool) (int64, error)
	// FreezeLiquidity will freeze a certain liquidity for specified amount until specified timestamp with an index of hash.
	FreezeLiquidity(chainId uint64, token eth.Addr, amount *big.Int, until int64, hash eth.Hash, isNative bool) error
	// UnfreezeLiquidity will try to unfreeze a certain liquidity with specified hash.
	UnfreezeLiquidity(chainId uint64, hash eth.Hash) error
	// DstTransfer should send tx on dstChain to transfer dstToken to user
	DstTransfer(transferNative bool, _quote rfq.RFQQuote, opts ...ethutils.TxOption) (eth.Hash, error)
	// SrcRelease should send tx on srcChain to release srcToken to mm
	SrcRelease(_quote rfq.RFQQuote, _execMsgCallData []byte, opts ...ethutils.TxOption) (eth.Hash, error)
}

type AmountCalculator interface {
	CalRecvAmt(tokenIn, tokenOut *common.Token, amountIn, baseFee *big.Int, isLightMM bool) (recvAmt, releaseAmt, fee *big.Int, err error)
	CalSendAmt(tokenIn, tokenOut *common.Token, amountOut *big.Int) (sendAmt, releaseAmt, fee *big.Int, err error)
}

type RequestSigner interface {
	Sign(data []byte) ([]byte, error)
	Verify(data, sig []byte) bool
}

func NewClient(server string, ops ...grpc.DialOption) *Client {
	conn, err := grpc.Dial(server, ops...)
	if err != nil {
		panic(err)
	}
	client := proto.NewApiClient(conn)
	return &Client{server: server, conn: conn, ApiClient: client}
}

func (c *Client) Close() {
	c.conn.Close()
}

func NewServer(config *ServerConfig, client *rfqserver.Client, cm ChainQuerier, lp LiquidityProvider, ac AmountCalculator, rs RequestSigner) *Server {
	// clean non-set config, except of token pair policy
	config.clean()
	// set up token pairs
	lp.SetupTokenPairs(config.TPPolicyList)
	return &Server{
		Ctl:               make(chan bool),
		RfqClient:         client,
		Config:            config,
		ChainCaller:       cm,
		LiquidityProvider: lp,
		AmountCalculator:  ac,
		RequestSigner:     rs,
	}
}

func (s *Server) Serve(ops ...grpc.ServerOption) {
	host := s.Config.Host
	if host == "" {
		host = "localhost"
	}
	log.Infof("Start mm server, listen on %s:%d", host, s.Config.PortListenOn)
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, s.Config.PortListenOn))
	if err != nil {
		panic(err)
	}
	grpcServer := grpc.NewServer(ops...)
	proto.RegisterApiServer(grpcServer, s)
	grpcServer.Serve(lis)
}

func (s *Server) ReportConfigs() {
	log.Infof("Start reporting token config to rfq server every %d seconds", s.Config.ReportRetryPeriod)
	tokens := s.LiquidityProvider.GetTokens()
	if len(tokens) == 0 {
		log.Panicf("No token config.")
	}
	log.Infof("Supported token list:")
	for _, token := range tokens {
		log.Infof("%s", token.String())
	}
	// report tokens to rfq server
	for {
		err := s.report(tokens)
		if err != nil {
			log.Errorf("Report token config err:%s", err)
			time.Sleep(time.Duration(s.Config.ReportRetryPeriod) * time.Second)
		} else {
			log.Infof("Report token config succeeded")
			return
		}
	}
}

func (s *Server) report(tokens []*common.Token) error {
	request := &rfqproto.UpdateConfigsRequest{Config: &proto.Config{Tokens: tokens}}
	_, err := s.RfqClient.UpdateConfigs(context.Background(), request)
	return err
}

func (s *Server) StopProcessing(reason string) {
	log.Infof("Stopping server from processing pending orders, because %s", reason)
	s.Ctl <- true
}

func (s *Server) DefaultProcessOrder() {
	log.Infof("Start processing order every %d seconds", s.Config.ProcessPeriod)
	go func() {
		if s.Ctl == nil {
			log.Panicln("nil control channel")
		}
		ticker := time.NewTicker(time.Duration(s.Config.ProcessPeriod) * time.Second)
		for {
			select {
			case <-ticker.C:
				// check component's functionality
				if s.LiquidityProvider.IsPaused() {
					s.StopProcessing("liquidity provider is paused in some reason")
					continue
				}
				resp, err := s.RfqClient.PendingOrders(context.Background(), &rfqproto.PendingOrdersRequest{})
				if err != nil {
					log.Errorf("PendingOrders err:%s", err)
					continue
				}
				s.processOrders(resp.Orders)
			case <-s.Ctl:
				return
			}
		}
	}()
}

func (s *Server) processOrders(orders []*rfqproto.PendingOrder) {
	var wg sync.WaitGroup
	for _, pendingOrder := range orders {
		wg.Add(1)
		go func(order *rfqproto.PendingOrder) {
			defer wg.Done()
			s.processOrder(order)
		}(pendingOrder)
	}
	wg.Wait()
}

func (s *Server) processOrder(pendingOrder *rfqproto.PendingOrder) {
	quote := pendingOrder.Quote
	err := s.checkQuoteSig(quote, pendingOrder.QuoteSig)
	if err != nil {
		return
	}
	quoteHash := quote.GetQuoteHash()
	switch pendingOrder.Status {
	case rfqproto.OrderStatus_STATUS_SRC_DEPOSITED:
		// check quote
		err = s.checkQuote(quote, pendingOrder.SrcDepositTxHash, true)
		if err != nil {
			return
		}
		// send dst transfer
		txHash, err := s.LiquidityProvider.DstTransfer(pendingOrder.DstNative, quote.ToQuoteOnChain())
		if err != nil {
			log.Errorf("DstTransfer err:%s, quoteHash %x", err, quoteHash)
			return
		}
		log.Infof("DstTransfer sent with txHash %x, quoteHash %x", txHash, quoteHash)
		// update order's status
		s.updateOrder(quoteHash, rfqproto.OrderStatus_STATUS_MM_DST_EXECUTED, eth.Bytes2Hex(txHash.Bytes()))
	case rfqproto.OrderStatus_STATUS_DST_TRANSFERRED:
		// 1. send src release
		txHash, err := s.LiquidityProvider.SrcRelease(quote.ToQuoteOnChain(), pendingOrder.ExecMsgCallData)
		if err != nil {
			log.Errorf("SrcRelease err:%s, quoteHash %x", err, quoteHash)
			return
		}
		log.Infof("SrcRelease sent with txHash %x, quoteHash %x", txHash, quoteHash)
		// 2. update order's status
		s.updateOrder(quoteHash, rfqproto.OrderStatus_STATUS_MM_SRC_EXECUTED, eth.Bytes2Hex(txHash.Bytes()))
	}
}

func (s *Server) checkQuoteSig(quote *proto.Quote, sig string) (err error) {
	quoteHash := quote.GetQuoteHash()
	if !s.ValidateQuote(quote, eth.Hex2Bytes(sig)) {
		err = fmt.Errorf("invalid quote, quoteHash %x", quoteHash)
		log.Errorln(err)
	}
	return
}

func (s *Server) checkQuote(quote *proto.Quote, srcDepositTxHash string, processOrder bool) error {
	quoteHash := quote.GetQuoteHash()
	// 1. check dst deadline
	timestamp := time.Now().Unix()
	if quote.DstDeadline < timestamp {
		msg := fmt.Sprintf("SrcDeposited order with hash %x has past dst deadline %s, now is %s.", quoteHash,
			time.Unix(quote.DstDeadline, 0).Format("2006-01-02 15:04:06"),
			time.Unix(timestamp, 0).Format("2006-01-02 15:04:06"))
		log.Infoln(msg)
		//s.unfreeze(quote)
		// same chain swap, update status to refund initiated
		if quote.GetSrcChainId() == quote.GetDstChainId() && processOrder {
			s.updateOrder(quoteHash, rfqproto.OrderStatus_STATUS_REFUND_INITIATED, "")
		}
		return fmt.Errorf(msg)
	}
	// 2. verify tx on src chain
	ok, err := s.ChainCaller.VerifyRfqEvent(quote.GetSrcChainId(), eth.Hex2Hash(srcDepositTxHash), rfq.EventNameSrcDeposited)
	if err != nil {
		msg := fmt.Sprintf("VerifyRfqEvent err:%s, quoteHash %x", err, quoteHash)
		log.Warnln(msg)
		return fmt.Errorf(msg)
	}
	if !ok {
		msg := fmt.Sprintf("[Serious] Quote(hash %x) with status SRC_DEPOSITED does not pass event verification", quoteHash)
		log.Errorln(msg)
		//s.unfreeze(quote)
		if processOrder {
			s.StopProcessing(fmt.Sprintf("the order with hash %x does not pass event verification", quoteHash))
		}
		return fmt.Errorf(msg)
	}
	// 3. check quoteHash on src chain
	statusOnChain, err := s.ChainCaller.GetQuoteStatus(quote.GetSrcChainId(), quoteHash)
	if err != nil {
		msg := fmt.Sprintf("GetQuoteStatus err:%s, quoteHash %x", err, quoteHash)
		log.Errorln(msg)
		return fmt.Errorf(msg)
	}
	if statusOnChain != rfq.QuoteStatusSrcDeposited {
		msg := fmt.Sprintf("[Serious] Quote(hash %x) status on src chain is %s, expected %s", quoteHash, rfq.GetQuoteStatusName(statusOnChain), rfq.GetQuoteStatusName(rfq.QuoteStatusSrcDeposited))
		log.Errorln(msg)
		//s.unfreeze(quote)
		if processOrder {
			s.StopProcessing(fmt.Sprintf("the order with hash %x is not truly deposited on src chain while rfq server thought it is", quoteHash))
		}
		return fmt.Errorf(msg)
	}
	return nil
}

func (s *Server) ValidateQuote(quote *proto.Quote, sig []byte) bool {
	// 1 check sig
	if !s.RequestSigner.Verify(quote.GetQuoteHash().Bytes(), sig) {
		// serious error
		log.Errorf("[Serious] Invalid sig, quoteHash %x", eth.Hex2Hash(quote.Hash))
		return false
	}
	// 2 check quote hash
	if quote.GetQuoteHash() != quote.EncodeQuoteHash() {
		// serious error
		log.Errorf("[Serious] Quote hash mismatch, got %x, calculated %x", eth.Hex2Hash(quote.Hash), quote.EncodeQuoteHash())
		return false
	}
	return true
}

func (s *Server) unfreeze(quote *proto.Quote) {
	err := s.LiquidityProvider.UnfreezeLiquidity(quote.GetDstChainId(), quote.GetQuoteHash())
	if err != nil {
		log.Errorf("UnfreezeLiquidity err:%s, quoteHash %x", err, eth.Hex2Hash(quote.Hash))
	}
}

func (s *Server) updateOrder(quoteHash eth.Hash, toStatus rfqproto.OrderStatus, txHash string) {
	_, err := s.RfqClient.UpdateOrders(context.Background(),
		&rfqproto.UpdateOrdersRequest{
			Updates: []*rfqproto.OrderUpdates{{QuoteHash: quoteHash.String(), OrderStatus: toStatus, ExecTxHash: txHash}},
		})
	if err != nil {
		log.Errorf("UpdateOrders err:%s, quoteHash %x, toStatus %s, txHash %s", err, quoteHash, toStatus, txHash)
	} else {
		log.Infof("Order updated, quoteHash %x, toStatus %s, txHash %s", quoteHash, toStatus, txHash)
	}
}
