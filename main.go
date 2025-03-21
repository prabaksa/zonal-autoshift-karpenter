package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// NodePool represents the structure of a Karpenter NodePool
type NodePool struct {
    APIVersion string `json:"apiVersion"`
    Kind       string `json:"kind"`
    Metadata   struct {
        Name      string            `json:"name"`
        Namespace string            `json:"namespace"`
        Labels    map[string]string `json:"labels,omitempty"`
    } `json:"metadata"`
    Spec struct {
        Template struct {
            Spec struct {
                Requirements []struct {
                    Key      string   `json:"key"`
                    Operator string   `json:"operator"`
                    Values   []string `json:"values"`
                } `json:"requirements"`
                NodeClassRef struct {
                    Name string `json:"name"`
					Kind string `json:"kind"`
					Group string `json:"group"`
                } `json:"nodeClassRef"`
            } `json:"spec"`
        } `json:"template"`
        Limits struct {
            CPU    string `json:"cpu,omitempty"`
            Memory string `json:"memory,omitempty"`
        } `json:"limits,omitempty"`
    } `json:"spec"`
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

// CreateNodePool creates a new Karpenter NodePool
func CreateNodePool(ctx context.Context, namespace string,nodepoolName string, nodePoolSpec []byte) error {
    k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("[CreateNodePool] Failed to create cluster config: %v", err)
		return fmt.Errorf("failed to create cluster config: %v", err)
	}
	
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		log.Printf("[CreateNodePool] Failed to create clientset: %v", err)
		return fmt.Errorf("Failed to create clientset")
	}
	
	
	resp, err := clientset.RESTClient().Post().AbsPath("/apis/karpenter.sh/v1/nodepools").Body(nodePoolSpec).DoRaw(context.TODO())
	if err != nil {
		log.Printf("[CreateNodePool] Failed to CreateNodePool node pools: %v", err)
		return fmt.Errorf("Failed to update node pools")
	}
	log.Printf("[CreateNodePool] Succesfully created node pools: %v", nodepoolName)
	resp_json, err := json.Marshal(resp)
	log.Printf("[CreateNodePool] Response: %v", resp_json)

	return nil
}


// Create a function to identity the updated Availability zones after ignoring the AwayFrom zone. 
func getUpdatedZones(event Event) ([]string) {
	region := event.Region
	awsCfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(region),
	)
	if err != nil {
		log.Printf("Failed to load AWS config: %v", err)
	}
	ec2Client := ec2.NewFromConfig(awsCfg)
	//get list of availability zones which are part of the region in event and store it in input variable
	input := &ec2.DescribeAvailabilityZonesInput{}
	output, err := ec2Client.DescribeAvailabilityZones(context.TODO(),input)
	//log.Printf("[updateKarpenterNodePool] Retrieved %d AZs from event", output.AvailabilityZones)
	log.Printf("[updateKarpenterNodePool] Retrieved %d AZs from event", output.AvailabilityZones)
	if err != nil {
		log.Printf("[updateKarpenterNodePool] Failed to describe availability zones: %v", err)
	}
	log.Printf("[updateKarpenterNodePool] Retrieved %d AZs from EC2 API", len(output.AvailabilityZones))
	//
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
	// Log the updated zones
	log.Printf("[updateKarpenterNodePool] Updated zones: %v", updatedZones)
	//return updatedZones to the calling function
	return updatedZones
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

	// Check if the length of the nodepool is 2 and the nodepool names are "general-purpose" and "system"
	if len(nodePoolList.Items) == 2 && 
	   ((nodePoolList.Items[0].Metadata.Name == "general-purpose" && nodePoolList.Items[1].Metadata.Name == "system") ||
	    (nodePoolList.Items[1].Metadata.Name == "general-purpose" && nodePoolList.Items[0].Metadata.Name == "system")) {
		log.Println("[updateKarpenterNodePool] Found 2 EKS Auto mode default node pools, creating a new node pool")
		//create a new node pool zonal-shift-karpenter copying the "general-purpose" nodepool
		// Create a new NodePool with the correct structure
		nodePoolItem := NodePool{
			APIVersion: "karpenter.sh/v1",
			Kind:       "NodePool",
			Metadata: struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels,omitempty"`
			}{
				Name: "zonal-shift-karpenter",
				Namespace: "default",
			},
		}
		
		// Copy over the requirements from the existing nodepool
		// but don't try to directly assign the Spec which has a different structure
		
		// Initialize the template and spec structures
		nodePoolItem.Spec.Template.Spec.Requirements = make([]struct {
			Key      string   `json:"key"`
			Operator string   `json:"operator"`
			Values   []string `json:"values"`
		}, 0)
		
		// Copy requirements from the existing nodepool
		for _, req := range nodePoolList.Items[0].Spec.Template.Spec.Requirements {
			nodePoolItem.Spec.Template.Spec.Requirements = append(
				nodePoolItem.Spec.Template.Spec.Requirements,
				struct {
					Key      string   `json:"key"`
					Operator string   `json:"operator"`
					Values   []string `json:"values"`
				}{
					Key:      req.Key,
					Operator: req.Operator,
					Values:   req.Values,
				},
			)
		}
		nodePoolItem.APIVersion = "karpenter.sh/v1"
		nodePoolItem.Kind = "NodePool"
		nodePoolItem.Metadata.Name = "zonal-shift-karpenter"
		
		// Copy the node class reference from the existing node pool
		nodePoolItem.Spec.Template.Spec.NodeClassRef.Name = "default"
		nodePoolItem.Spec.Template.Spec.NodeClassRef.Kind = "NodeClass"
		nodePoolItem.Spec.Template.Spec.NodeClassRef.Group = "eks.amazonaws.com"

		log.Printf("[updateKarpenterNodePool] Calling function getUpdatedZones to get healthy zones")
		updatedZones := getUpdatedZones(event)
		
		// Update the requirements with the new zones
		zoneRequirementExists := false
		for i, req := range nodePoolItem.Spec.Template.Spec.Requirements {
			if req.Key == "topology.kubernetes.io/zone" {
				nodePoolItem.Spec.Template.Spec.Requirements[i].Values = updatedZones
				zoneRequirementExists = true
				break
			}
		}
		
		// If no zone requirement exists, add one
		if !zoneRequirementExists {
			nodePoolItem.Spec.Template.Spec.Requirements = append(nodePoolItem.Spec.Template.Spec.Requirements, struct {
				Key      string   `json:"key"`
				Operator string   `json:"operator"`
				Values   []string `json:"values"`
			}{
				Key:      "topology.kubernetes.io/zone",
				Operator: "In",
				Values:   updatedZones,
			})
		}
		
		// Convert nodePoolItem to JSON
		newNodePoolJSON, err := json.Marshal(nodePoolItem)
		if err != nil {
			log.Printf("[updateKarpenterNodePool] Failed to marshal new node pool: %v", err)
			return
		}
		log.Printf("[updateKarpenterNodePool] Creating new node pool: %s", nodePoolItem.Metadata.Name)
		ctx := context.Background()
		err = CreateNodePool(ctx, "default", "zonal-shift-karpenter", newNodePoolJSON)
		} else {
		for _, pool := range nodePoolList.Items {
			log.Printf("[updateKarpenterNodePool] Processing node pool: %s", pool.Metadata.Name)
			log.Printf("[updateKarpenterNodePool] Number of requirements: %d", len(pool.Spec.Template.Spec.Requirements))
			for i, req := range pool.Spec.Template.Spec.Requirements {
				log.Printf("[updateKarpenterNodePool] Checking requirement %d: Key=%s", i, req.Key)
				if req.Key == "topology.kubernetes.io/zone" {
					log.Printf("[updateKarpenterNodePool] Node pool %s has a zone requirement: %+v", pool.Metadata.Name, req.Values)
					updatedZones := getUpdatedZones(event)
					if len(updatedZones) != len(req.Values) {
						log.Printf("[updateKarpenterNodePool] Zone list changed for node pool %s:", pool.Metadata.Name)
						log.Printf("[updateKarpenterNodePool] Original zones: %v", req.Values)
						log.Printf("[updateKarpenterNodePool] Updated zones: %v", updatedZones)
						log.Printf("[updateKarpenterNodePool] Updating node pool %s to remove AZ %s",
							pool.Metadata.Name, event.Detail.Metadata.AwayFrom)
						pool.Spec.Template.Spec.Requirements[i].Values = updatedZones
						//Update the Nodepool with the updatedzones using clientset
						pool_json, err := json.Marshal(pool)
						if err != nil {
							log.Printf("[updateKarpenterNodePool] Failed to parse Nodepool : %v", err)
							return (err)
						}
						log.Printf("[CreateNodePool] pool_json: %v", pool_json)
						resp_update, err := clientset.RESTClient().Put().AbsPath("/apis/karpenter.sh/v1/nodepools").Body(pool_json).DoRaw(context.TODO())
						if err != nil {
							log.Printf("[updateKarpenterNodePool] Failed to update node pools: %v", err)
							return (err)
						}
						resp_update_json, err := json.Marshal(resp_update)
						log.Printf("[CreateNodePool] Response: %v", resp_update_json)
						
		// No need to set NodeClassRef here as it's not accessible in this struct
					} else {
						log.Printf("[updateKarpenterNodePool] No changes needed for node pool %s - zones unchanged",
							pool.Metadata.Name)
					}
				}
			}
		}
	}
}