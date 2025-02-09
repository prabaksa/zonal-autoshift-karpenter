package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	//"io"
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// SNSMessage represents the structure of an SNS notification
type SNSMessage struct {
	Type             string `json:"Type"`
	MessageId        string `json:"MessageId"`
	TopicArn         string `json:"TopicArn"`
	Message          string `json:"Message"`
	SubscribeURL     string `json:"SubscribeURL"`
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
}

type Event struct {
	Version    string   `json:"version"`
	ID         string   `json:"id"`
	DetailType string   `json:"detail-type"`
	Source     string   `json:"source"`
	Account    string   `json:"account"`
	Time       string   `json:"time"`
	Region     string   `json:"region"`
	Resources  []string `json:"resources"`
	Detail     Detail   `json:"detail"`
}

type Detail struct {
	Version  string   `json:"version"`
	Data     string   `json:"data"`
	Metadata Metadata `json:"metadata"`
}

type Metadata struct {
	AwayFrom string `json:"awayFrom"`
	Notes    string `json:"notes"`
}

func init() {
	// Open log file
	logFile, err := os.OpenFile("/var/log/zonal-shift.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}

	// Configure logger to write to file
	log.SetOutput(logFile)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

func main() {
	// Creates a gin router with default middleware (logger and recovery)
	router := gin.Default()

	// Register your handler
	router.POST("/sns", handleSNS)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Start the server
	if err := router.Run(":" + port); err != nil {
		fmt.Printf("Failed to start server: %v\n", err)
		os.Exit(1)
	}
}
func handleSNS(c *gin.Context) {
	log.Println("[handleSNS] Request received")

	// Read the raw body
	body, err := c.GetRawData()
	if err != nil {
		log.Printf("[handleSNS] Error reading body: %v", err)
		c.String(http.StatusBadRequest, "Error reading request")
		return
	}

	log.Printf("[handleSNS] Raw body received: %s", string(body))

	// First try to parse as SNS message
	var snsMessage SNSMessage
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&snsMessage); err != nil {
		log.Printf("[handleSNS] Not an SNS message, trying direct event format: %v", err)
	} else if snsMessage.Type != "" {
		// Handle SNS message
		if snsMessage.Type == "SubscriptionConfirmation" {
			log.Println("[handleSNS] Processing subscription confirmation")
			resp, err := http.Get(snsMessage.SubscribeURL)
			if err != nil {
				log.Printf("[handleSNS] Subscription confirmation failed: %v", err)
				c.String(http.StatusInternalServerError, "Failed to confirm subscription")
				return
			}
			defer resp.Body.Close()
			log.Println("[handleSNS] Subscription confirmed successfully")
			c.Status(http.StatusOK)
			return
		}

		if snsMessage.Type == "Notification" {
			log.Println("[handleSNS] Processing SNS notification")
			var event Event
			if err := json.Unmarshal([]byte(snsMessage.Message), &event); err != nil {
				log.Printf("[handleSNS] Failed to parse event from SNS message: %v", err)
				c.String(http.StatusBadRequest, "Invalid event format in SNS message")
				return
			}
			handleEvent(event)
			c.Status(http.StatusOK)
			return
		}
	}

	// Try parsing as direct EventBridge event
	var event Event
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&event); err != nil {
		log.Printf("[handleSNS] Failed to parse as direct event: %v", err)
		c.String(http.StatusBadRequest, "Invalid message format")
		return
	}

	log.Printf("[handleSNS] Direct event received - ID: %s, Type: %s, AZ: %s",
		event.ID,
		event.DetailType,
		event.Detail.Metadata.AwayFrom)

	handleEvent(event)
	c.Status(http.StatusOK)
}

// handleEvent processes the event regardless of how it was received
func handleEvent(event Event) {
	go func() {
		log.Println("[handleSNS] Starting updateKarpenterNodePool")
		updateKarpenterNodePool(event)
		log.Println("[handleSNS] Completed updateKarpenterNodePool")
	}()
}

// updateKarpenterNodePool updates the Karpenter node pool based on the event
func updateKarpenterNodePool(event Event) {
	log.Printf("[updateKarpenterNodePool] Processing event for AZ: %s", event.Detail.Metadata.AwayFrom)

	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("[updateKarpenterNodePool] Failed to create cluster config: %v", err)
		return
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		log.Printf("[updateKarpenterNodePool] Failed to create clientset: %v", err)
		return
	}

	log.Println("[updateKarpenterNodePool] Retrieving Karpenter node pools...")
	nodePools, err := clientset.RESTClient().Get().AbsPath("/apis/karpenter.sh/v1/nodepools").DoRaw(context.TODO())
	if err != nil {
		log.Printf("[updateKarpenterNodePool] Failed to get node pools: %v", err)
		return
	}

	var nodePoolList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Requirements []struct {
							Key      string   `json:"key"`
							Operator string   `json:"operator"`
							Values   []string `json:"values"`
						} `json:"requirements"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}

	if err := json.Unmarshal(nodePools, &nodePoolList); err != nil {
		log.Printf("[updateKarpenterNodePool] Failed to parse node pools: %v", err)
		return
	}

	log.Printf("[updateKarpenterNodePool] Found %d node pools", len(nodePoolList.Items))
	// Let's log the raw JSON for debugging
	rawJSON, _ := json.MarshalIndent(nodePoolList, "", "  ")
	log.Printf("[updateKarpenterNodePool] Raw node pool list: %s", string(rawJSON))

	awsCfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Printf("Failed to load AWS config: %v", err)
		return
	}

	ec2Client := ec2.NewFromConfig(awsCfg)

	for _, pool := range nodePoolList.Items {
		log.Printf("[updateKarpenterNodePool] Processing node pool: %s", pool.Metadata.Name)
		log.Printf("[updateKarpenterNodePool] Number of requirements: %d", len(pool.Spec.Template.Spec.Requirements))
		for i, req := range pool.Spec.Template.Spec.Requirements {
			log.Printf("[updateKarpenterNodePool] Checking requirement %d: Key=%s", i, req.Key)
			if req.Key == "topology.kubernetes.io/zone" {
				log.Printf("[updateKarpenterNodePool] Node pool %s has a zone requirement: %+v", pool.Metadata.Name, req.Values)
				input := &ec2.DescribeAvailabilityZonesInput{
					ZoneNames: req.Values,
				}
				output, err := ec2Client.DescribeAvailabilityZones(context.TODO(), input)
				if err != nil {
					log.Printf("[updateKarpenterNodePool] Failed to describe availability zones: %v", err)
					continue
				}
				log.Printf("[updateKarpenterNodePool] Retrieved %d AZs from EC2 API", len(output.AvailabilityZones))
				var updatedZones []string
				for _, az := range output.AvailabilityZones {
					log.Printf("[updateKarpenterNodePool] Checking AZ: ZoneId=%s, ZoneName=%s", *az.ZoneId, *az.ZoneName)
					if *az.ZoneId != event.Detail.Metadata.AwayFrom {
						log.Printf("[updateKarpenterNodePool] Including AZ %s in updated zones", *az.ZoneName)
						updatedZones = append(updatedZones, *az.ZoneName)
					} else {
						log.Printf("[updateKarpenterNodePool] Excluding AZ %s as it matches AwayFrom zone", *az.ZoneName)
					}
				}

				if len(updatedZones) != len(req.Values) {
					log.Printf("[updateKarpenterNodePool] Zone list changed for node pool %s:", pool.Metadata.Name)
					log.Printf("[updateKarpenterNodePool] Original zones: %v", req.Values)
					log.Printf("[updateKarpenterNodePool] Updated zones: %v", updatedZones)
					log.Printf("[updateKarpenterNodePool] Updating node pool %s to remove AZ %s",
						pool.Metadata.Name, event.Detail.Metadata.AwayFrom)
					pool.Spec.Template.Spec.Requirements[i].Values = updatedZones
				} else {
					log.Printf("[updateKarpenterNodePool] No changes needed for node pool %s - zones unchanged",
						pool.Metadata.Name)
				}
			}
		}
	}
}
