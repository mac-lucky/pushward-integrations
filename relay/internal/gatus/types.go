package gatus

type webhookPayload struct {
	EndpointName  string `json:"endpoint_name"`
	EndpointGroup string `json:"endpoint_group"`
	EndpointURL   string `json:"endpoint_url"`
	Description   string `json:"alert_description"`
	Status        string `json:"status"`
	ResultErrors  string `json:"result_errors"`
}
