/*-
 * Copyright (c) 2016-2021, F5 Networks, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package crmanager

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	rsc "github.com/F5Networks/k8s-bigip-ctlr/pkg/resource"
	log "github.com/F5Networks/k8s-bigip-ctlr/pkg/vlogger"
	"github.com/F5Networks/k8s-bigip-ctlr/pkg/writer"
)

const (
	as3SharedApplication = "Shared"

	baseAS3Config = `{
  "$schema": "https://raw.githubusercontent.com/F5Networks/f5-appsvcs-extension/master/schema/3.18.0/as3-schema-3.18.0-4.json",
  "class": "AS3",
  "declaration": {
    "class": "ADC",
    "schemaVersion": "3.18.0",
    "id": "urn:uuid:B97DFADF-9F0D-4F6C-8D66-E9B52E593694",
    "label": "CIS Declaration",
	"remark": "Auto-generated by CIS"
  }
}
`
)

var DEFAULT_PARTITION string

func NewAgent(params AgentParams) *Agent {
	DEFAULT_PARTITION = params.Partition
	postMgr := NewPostManager(params.PostParams)
	configWriter, err := writer.NewConfigWriter()
	if nil != err {
		log.Fatalf("Failed creating ConfigWriter tool: %v", err)
	}
	agent := &Agent{
		PostManager:  postMgr,
		Partition:    params.Partition,
		ConfigWriter: configWriter,
		EventChan:    make(chan interface{}),
		activeDecl:   "",
		userAgent:    params.UserAgent,
	}
	// If running in VXLAN mode, extract the partition name from the tunnel
	// to be used in configuring a net instance of CCCL for that partition
	var vxlanPartition string
	if len(params.VXLANName) > 0 {
		cleanPath := strings.TrimLeft(params.VXLANName, "/")
		slashPos := strings.Index(cleanPath, "/")
		if slashPos == -1 {
			// No partition
			vxlanPartition = "Common"
		} else {
			// Partition and name
			vxlanPartition = cleanPath[:slashPos]
		}
	}

	gs := globalSection{
		LogLevel:       params.LogLevel,
		VerifyInterval: params.VerifyInterval,
		VXLANPartition: vxlanPartition,
		DisableLTM:     true,
		GTM:            true,
	}
	bs := bigIPSection{
		BigIPUsername:   params.PostParams.BIGIPUsername,
		BigIPPassword:   params.PostParams.BIGIPPassword,
		BigIPURL:        params.PostParams.BIGIPURL,
		BigIPPartitions: []string{params.Partition},
	}

	var gtm gtmBigIPSection
	if len(params.GTMParams.GTMBigIpUsername) == 0 || len(params.GTMParams.GTMBigIpPassword) == 0 || len(params.GTMParams.GTMBigIpUrl) == 0 {
		gs.GTM = false
	} else {
		gtm = gtmBigIPSection{
			GtmBigIPUsername: params.GTMParams.GTMBigIpUsername,
			GtmBigIPPassword: params.GTMParams.GTMBigIpPassword,
			GtmBigIPURL:      params.GTMParams.GTMBigIpUrl,
		}
	}

	agent.startPythonDriver(
		gs,
		bs,
		gtm,
		params.PythonBaseDir,
	)

	return agent
}

func (agent *Agent) Stop() {
	agent.ConfigWriter.Stop()
	agent.stopPythonDriver()
}

func (agent *Agent) PostConfig(config ResourceConfigWrapper) {
	agent.PostGTMConfig(config)
	decl := createAS3Declaration(config, agent.userAgent)
	if DeepEqualJSON(agent.activeDecl, decl) {
		log.Debug("[AS3] No Change in the Configuration")
		return
	}
	agent.Write(string(decl), nil)
	agent.activeDecl = decl

	allPoolMembers := config.rsCfgs.GetAllPoolMembers()

	// Convert allPoolMembers to rsc.Members so that vxlan Manger accepts
	var allPoolMems []rsc.Member

	for _, poolMem := range allPoolMembers {
		allPoolMems = append(
			allPoolMems,
			rsc.Member(poolMem),
		)
	}
	if agent.EventChan != nil {
		select {
		case agent.EventChan <- allPoolMems:
			log.Debugf("Custom Resource Manager wrote endpoints to VxlanMgr")
		case <-time.After(3 * time.Second):
		}
	}
}

func (agent Agent) PostGTMConfig(config ResourceConfigWrapper) {

	dnsConfig := make(map[string]interface{})
	wideIPs := WideIPs{}
	for _, v := range config.dnsConfig {
		wideIPs.WideIPs = append(wideIPs.WideIPs, v)
	}

	// TODO: Need to change to DEFAULT_PARTITION from Common, once Agent starts to support DEFAULT_PARTITION
	dnsConfig["Common"] = wideIPs

	doneCh, errCh, err := agent.ConfigWriter.SendSection("gtm", dnsConfig)

	if nil != err {
		log.Warningf("Failed to write gtm config section: %v", err)
	} else {
		select {
		case <-doneCh:
			log.Debugf("Wrote gtm config section: %v", config.dnsConfig)
		case e := <-errCh:
			log.Warningf("Failed to write gtm config section: %v", e)
		case <-time.After(time.Second):
			log.Warningf("Did not receive write response in 1s")
		}
	}
}

//Create AS3 declaration
func createAS3Declaration(config ResourceConfigWrapper, userAgentInfo string) as3Declaration {
	var as3Config map[string]interface{}
	_ = json.Unmarshal([]byte(baseAS3Config), &as3Config)

	adc := as3Config["declaration"].(map[string]interface{})
	for k, v := range createAS3ADC(config) {
		adc[k] = v
	}

	controlObj := make(map[string]interface{})
	controlObj["class"] = "Controls"
	controlObj["userAgent"] = userAgentInfo
	adc["controls"] = controlObj

	decl, err := json.Marshal(as3Config)
	if err != nil {
		log.Debugf("[AS3] Unified declaration: %v\n", err)
	}
	return as3Declaration(decl)
}

func createAS3ADC(config ResourceConfigWrapper) as3ADC {
	// Create Shared as3Application object
	sharedApp := as3Application{}
	sharedApp["class"] = "Application"
	sharedApp["template"] = "shared"
	// Process rscfg to create AS3 Resources
	processResourcesForAS3(config.rsCfgs, sharedApp, config.shareNodes)

	// Process CustomProfiles
	processCustomProfilesForAS3(config.customProfiles, sharedApp)

	// Process Profiles
	processProfilesForAS3(config.rsCfgs, sharedApp)

	processIRulesForAS3(config.iRuleMap, sharedApp)

	processDataGroupForAS3(config.intDgMap, sharedApp)

	// Create AS3 Tenant
	tenant := as3Tenant{
		"class":              "Tenant",
		as3SharedApplication: sharedApp,
	}
	as3JSONDecl := as3ADC{
		DEFAULT_PARTITION: tenant,
	}
	return as3JSONDecl
}

func processIRulesForAS3(iRuleMao IRulesMap, sharedApp as3Application) {
	// Create irule declaration
	for _, v := range iRuleMao {
		iRule := &as3IRules{}
		iRule.Class = "iRule"
		iRule.IRule = v.Code
		sharedApp[v.Name] = iRule
	}
}

func processDataGroupForAS3(intDgMap InternalDataGroupMap, sharedApp as3Application) {
	for idk, idg := range intDgMap {
		for _, dg := range idg {
			dataGroupRecord, found := sharedApp[dg.Name]
			if !found {
				dgMap := &as3DataGroup{}
				dgMap.Class = "Data_Group"
				dgMap.KeyDataType = "string"
				for _, record := range dg.Records {
					var rec as3Record
					rec.Key = record.Name
					virtualAddress := extractVirtualAddress(record.Data)
					// To override default Value created for CCCL for certain DG types
					if val, ok := getDGRecordValueForAS3(idk.Name, sharedApp, virtualAddress); ok {
						rec.Value = val
					} else {
						rec.Value = record.Data
					}
					dgMap.Records = append(dgMap.Records, rec)
				}
				// sort above create dgMap records.
				sort.Slice(dgMap.Records, func(i, j int) bool { return (dgMap.Records[i].Key < dgMap.Records[j].Key) })
				sharedApp[dg.Name] = dgMap
			} else {
				for _, record := range dg.Records {
					var rec as3Record
					rec.Key = record.Name
					virtualAddress := extractVirtualAddress(record.Data)
					// To override default Value created for CCCL for certain DG types
					if val, ok := getDGRecordValueForAS3(idk.Name, sharedApp, virtualAddress); ok {
						rec.Value = val
					} else {
						rec.Value = record.Data
					}
					sharedApp[dg.Name].(*as3DataGroup).Records = append(dataGroupRecord.(*as3DataGroup).Records, rec)
				}
				// sort above created
				sort.Slice(sharedApp[dg.Name].(*as3DataGroup).Records,
					func(i, j int) bool {
						return (sharedApp[dg.Name].(*as3DataGroup).Records[i].Key <
							sharedApp[dg.Name].(*as3DataGroup).Records[j].Key)
					})
			}
		}
	}
}

func extractVirtualAddress(str string) string {
	var address string
	if strings.HasPrefix(str, "crd_") && strings.HasSuffix(str, "_tls_client") {
		address = strings.ReplaceAll(strings.TrimRight(strings.TrimLeft(str, "crd_"), "_tls_client"), "_", ".")
	}
	return address
}

func getDGRecordValueForAS3(dgName string, sharedApp as3Application, virtualAddress string) (string, bool) {
	if strings.HasSuffix(dgName, ReencryptServerSslDgName) {
		for _, v := range sharedApp {
			if svc, ok := v.(*as3Service); ok && svc.Class == "Service_HTTPS" &&
				svc.VirtualAddresses[0] == virtualAddress {
				if val, ok := svc.ClientTLS.(*as3ResourcePointer); ok {
					return val.BigIP, true
				}
				if val, ok := svc.ClientTLS.(string); ok {
					return strings.Join([]string{"", val}, ""), true
				}
				log.Errorf("Unable to find serverssl for Data Group: %v\n", dgName)
			}
		}
	}
	return "", false
}

//Process for AS3 Resource
func processResourcesForAS3(rsCfgs ResourceConfigs, sharedApp as3Application, shareNodes bool) {
	for _, cfg := range rsCfgs {
		//Create policies
		createPoliciesDecl(cfg, sharedApp)

		//Create health monitor declaration
		createMonitorDecl(cfg, sharedApp)

		//Create pools
		createPoolDecl(cfg, sharedApp, shareNodes)

		switch cfg.MetaData.ResourceType {
		case VirtualServer:
			//Create AS3 Service for virtual server
			createServiceDecl(cfg, sharedApp)
		case TransportServer:
			//Create AS3 Service for transport virtual server
			createTransportServiceDecl(cfg, sharedApp)
		}

	}
}

//Create policy declaration
func createPoliciesDecl(cfg *ResourceConfig, sharedApp as3Application) {
	_, port := extractVirtualAddressAndPort(cfg.Virtual.Destination)
	for _, pl := range cfg.Policies {
		//Create EndpointPolicy
		ep := &as3EndpointPolicy{}
		for _, rl := range pl.Rules {

			ep.Class = "Endpoint_Policy"
			s := strings.Split(pl.Strategy, "/")
			ep.Strategy = s[len(s)-1]

			//Create rules
			rulesData := &as3Rule{Name: rl.Name}

			//Create condition object
			createRuleCondition(rl, rulesData, port)

			//Creat action object
			createRuleAction(rl, rulesData)

			ep.Rules = append(ep.Rules, rulesData)
		}
		//Setting Endpoint_Policy Name
		sharedApp[pl.Name] = ep
	}
}

// Create AS3 Pools for CRD
func createPoolDecl(cfg *ResourceConfig, sharedApp as3Application, shareNodes bool) {
	for _, v := range cfg.Pools {
		pool := &as3Pool{}
		// TODO
		// pool.LoadBalancingMode = v.Balance
		pool.Class = "Pool"
		for _, val := range v.Members {
			var member as3PoolMember
			member.AddressDiscovery = "static"
			member.ServicePort = val.Port
			member.ServerAddresses = append(member.ServerAddresses, val.Address)
			if shareNodes {
				member.ShareNodes = shareNodes
			}
			pool.Members = append(pool.Members, member)
		}
		for _, val := range v.MonitorNames {
			var monitor as3ResourcePointer
			use := strings.Split(val, "/")
			monitor.Use = fmt.Sprintf("/%s/%s/%s",
				DEFAULT_PARTITION,
				as3SharedApplication,
				use[len(use)-1],
			)
			pool.Monitors = append(pool.Monitors, monitor)
		}
		sharedApp[v.Name] = pool
	}
}

func updateVirtualToHTTPS(v *as3Service) {
	v.Class = "Service_HTTPS"
	redirect80 := false
	v.Redirect80 = &redirect80
}

// Process Irules for CRD
func processIrulesForCRD(cfg *ResourceConfig, svc *as3Service) {
	var IRules []interface{}
	for _, v := range cfg.Virtual.IRules {
		splits := strings.Split(v, "/")
		iRuleName := splits[len(splits)-1]

		var iRuleNoPort string
		lastIndex := strings.LastIndex(iRuleName, "_")
		if lastIndex > 0 {
			iRuleNoPort = iRuleName[:lastIndex]
		} else {
			iRuleNoPort = iRuleName
		}
		if strings.HasSuffix(iRuleNoPort, HttpRedirectIRuleName) ||
			strings.HasSuffix(iRuleNoPort, HttpRedirectNoHostIRuleName) ||
			strings.HasSuffix(iRuleName, TLSIRuleName) {

			IRules = append(IRules, iRuleName)
		} else {
			irule := &as3ResourcePointer{
				BigIP: v,
			}
			IRules = append(IRules, irule)
		}
		svc.IRules = IRules
	}
}

// Create AS3 Service for CRD
func createServiceDecl(cfg *ResourceConfig, sharedApp as3Application) {
	svc := &as3Service{}
	numPolicies := len(cfg.Virtual.Policies)
	switch {
	case numPolicies == 1:
		policyName := cfg.Virtual.Policies[0].Name
		svc.PolicyEndpoint = fmt.Sprintf("/%s/%s/%s",
			DEFAULT_PARTITION,
			as3SharedApplication,
			policyName)
	case numPolicies > 1:
		var peps []as3ResourcePointer
		for _, pep := range cfg.Virtual.Policies {
			peps = append(
				peps,
				as3ResourcePointer{
					Use: fmt.Sprintf("/%s/%s/%s",
						DEFAULT_PARTITION,
						as3SharedApplication,
						pep.Name,
					),
				},
			)
		}
		svc.PolicyEndpoint = peps
	case numPolicies == 0:
		// No policies since we need to handle the pool name.
		ps := strings.Split(cfg.Virtual.PoolName, "/")
		if cfg.Virtual.PoolName != "" {
			svc.Pool = fmt.Sprintf("/%s/%s/%s",
				DEFAULT_PARTITION,
				as3SharedApplication,
				ps[len(ps)-1])
		}
	}

	if len(cfg.Virtual.PersistenceMethods) == 0 {
		svc.Layer4 = cfg.Virtual.IpProtocol
		svc.Source = "0.0.0.0/0"
		svc.TranslateServerAddress = true
		svc.TranslateServerPort = true
		svc.Class = "Service_HTTP"
	} else {
		svc.PersistenceMethods = cfg.Virtual.PersistenceMethods
		svc.Class = "Service_TCP"
	}

	if cfg.Virtual.SNAT == "auto" || cfg.Virtual.SNAT == "none" {
		svc.SNAT = cfg.Virtual.SNAT
	} else {
		svc.SNAT = &as3ResourcePointer{
			BigIP: fmt.Sprintf("%v", cfg.Virtual.SNAT),
		}
	}

	//Attaching WAF policy
	if cfg.Virtual.WAF != "" {
		svc.WAF = &as3ResourcePointer{
			BigIP: fmt.Sprintf("%v", cfg.Virtual.WAF),
		}
	}
	//Attach allowVlans if exist.
	var vlans []as3ResourcePointer
	for _, va := range cfg.Virtual.AllowVLANs {
		vlans = append(
			vlans,
			as3ResourcePointer{
				BigIP: fmt.Sprintf("%v", va),
			},
		)
	}
	svc.AllowVLANs = vlans

	virtualAddress, port := extractVirtualAddressAndPort(cfg.Virtual.Destination)
	// verify that ip address and port exists.
	if virtualAddress != "" && port != 0 {
		va := append(svc.VirtualAddresses, virtualAddress)
		svc.VirtualAddresses = va
		svc.VirtualPort = port
	}
	//process irules for crd
	processIrulesForCRD(cfg, svc)
	sharedApp[cfg.Virtual.Name] = svc
}

// Create AS3 Rule Condition for CRD
func createRuleCondition(rl *Rule, rulesData *as3Rule, port int) {
	for _, c := range rl.Conditions {
		condition := &as3Condition{}
		if c.SSLExtensionClient {
			condition.Name = "host"
			condition.Type = "sslExtension"
			condition.Event = "ssl-client-hello"

			// For ports other then 80 and 443, attaching port number to host.
			// Ex. example.com:8080
			if port != 80 && port != 443 {
				var values []string
				for i := range c.Values {
					val := c.Values[i] + ":" + strconv.Itoa(port)
					values = append(values, val)
				}
				condition.ServerName = &as3PolicyCompareString{
					Values: values,
				}
			} else {
				condition.ServerName = &as3PolicyCompareString{
					Values: c.Values,
				}
			}
			if c.Equals {
				condition.ServerName.Operand = "equals"
			}
			rulesData.Conditions = append(rulesData.Conditions, condition)
			continue
		}

		if c.Host {
			condition.Name = "host"
			var values []string
			// For ports other then 80 and 443, attaching port number to host.
			// Ex. example.com:8080
			if port != 80 && port != 443 {
				for i := range c.Values {
					val := c.Values[i] + ":" + strconv.Itoa(port)
					values = append(values, val)
				}
				condition.All = &as3PolicyCompareString{
					Values: values,
				}
			} else {
				condition.All = &as3PolicyCompareString{
					Values: c.Values,
				}
			}
			if c.HTTPHost {
				condition.Type = "httpHeader"
			}
			if c.Equals {
				condition.All.Operand = "equals"
			}
		} else if c.PathSegment {
			condition.PathSegment = &as3PolicyCompareString{
				Values: c.Values,
			}
			if c.Name != "" {
				condition.Name = c.Name
			}
			condition.Index = c.Index
			if c.HTTPURI {
				condition.Type = "httpUri"
			}
			if c.Equals {
				condition.PathSegment.Operand = "equals"
			}
		} else if c.Path {
			condition.Path = &as3PolicyCompareString{
				Values: c.Values,
			}
			if c.Name != "" {
				condition.Name = c.Name
			}
			condition.Index = c.Index
			if c.HTTPURI {
				condition.Type = "httpUri"
			}
			if c.Equals {
				condition.Path.Operand = "equals"
			}
		}
		if c.Request {
			condition.Event = "request"
		}

		rulesData.Conditions = append(rulesData.Conditions, condition)
	}
}

// Create AS3 Rule Action for CRD
func createRuleAction(rl *Rule, rulesData *as3Rule) {
	for _, v := range rl.Actions {
		action := &as3Action{}
		if v.Forward {
			action.Type = "forward"
		}
		if v.Request {
			action.Event = "request"
		}
		if v.Redirect {
			action.Type = "httpRedirect"
		}
		if v.HTTPHost {
			action.Type = "httpHeader"
		}
		if v.HTTPURI {
			action.Type = "httpUri"
		}
		if v.Location != "" {
			action.Location = v.Location
		}
		// Handle hostname rewrite.
		if v.Replace && v.HTTPHost {
			action.Replace = &as3ActionReplaceMap{
				Value: v.Value,
				Name:  "host",
			}
		}
		// handle uri rewrite.
		if v.Replace && v.HTTPURI {
			action.Replace = &as3ActionReplaceMap{
				Value: v.Value,
			}
		}
		p := strings.Split(v.Pool, "/")
		if v.Pool != "" {
			action.Select = &as3ActionForwardSelect{
				Pool: &as3ResourcePointer{
					Use: p[len(p)-1],
				},
			}
		}
		rulesData.Actions = append(rulesData.Actions, action)
	}
}

//Extract virtual address and port from host URL
func extractVirtualAddressAndPort(str string) (string, int) {
	destination := strings.Split(str, "/")
	ipPort := strings.Split(destination[len(destination)-1], ":")
	// verify that ip address and port exists else log error.
	if len(ipPort) == 2 {
		port, _ := strconv.Atoi(ipPort[1])
		return ipPort[0], port
	} else {
		log.Error("Invalid Virtual Server Destination IP address/Port.")
		return "", 0
	}

}

func DeepEqualJSON(decl1, decl2 as3Declaration) bool {
	if decl1 == "" && decl2 == "" {
		return true
	}
	var o1, o2 interface{}

	err := json.Unmarshal([]byte(decl1), &o1)
	if err != nil {
		return false
	}

	err = json.Unmarshal([]byte(decl2), &o2)
	if err != nil {
		return false
	}

	return reflect.DeepEqual(o1, o2)
}

func processProfilesForAS3(rsCfgs ResourceConfigs, sharedApp as3Application) {
	for _, cfg := range rsCfgs {
		if svc, ok := sharedApp[cfg.Virtual.Name].(*as3Service); ok {
			processTLSProfilesForAS3(&cfg.Virtual, svc, cfg.Virtual.Name)
		}
	}
}

func processTLSProfilesForAS3(virtual *Virtual, svc *as3Service, profileName string) {
	// lets discard BIGIP profile creation when there exists a custom profile.
	as3ClientSuffix := "_tls_client"
	as3ServerSuffix := "_tls_server"
	for _, profile := range virtual.Profiles {
		switch profile.Context {
		case CustomProfileClient:
			// Profile is stored in a k8s secret
			if profile.Partition == "" {
				// Incoming traffic (clientssl) from a web client will be handled by ServerTLS in AS3
				svc.ServerTLS = fmt.Sprintf("/%v/%v/%v%v", DEFAULT_PARTITION,
					as3SharedApplication, profileName, as3ServerSuffix)

			} else {
				// Profile is a BIG-IP reference
				// Incoming traffic (clientssl) from a web client will be handled by ServerTLS in AS3
				svc.ServerTLS = &as3ResourcePointer{
					BigIP: fmt.Sprintf("/%v/%v", profile.Partition, profile.Name),
				}
			}
			updateVirtualToHTTPS(svc)
		case CustomProfileServer:
			// Profile is stored in a k8s secret
			if profile.Partition == "" {
				// Outgoing traffic (serverssl) to BackEnd Servers from BigIP will be handled by ClientTLS in AS3
				svc.ClientTLS = fmt.Sprintf("/%v/%v/%v%v", DEFAULT_PARTITION,
					as3SharedApplication, profileName, as3ClientSuffix)
			} else {
				// Profile is a BIG-IP reference
				// Outgoing traffic (serverssl) to BackEnd Servers from BigIP will be handled by ClientTLS in AS3
				svc.ClientTLS = &as3ResourcePointer{
					BigIP: fmt.Sprintf("/%v/%v", profile.Partition, profile.Name),
				}
			}
			updateVirtualToHTTPS(svc)
		}
	}
}

func processCustomProfilesForAS3(customProfiles *CustomProfileStore, sharedApp as3Application) {
	caBundleName := "serverssl_ca_bundle"
	var tlsClient *as3TLSClient
	// TLS Certificates are available in CustomProfiles
	for key, prof := range customProfiles.Profs {
		// Create TLSServer and Certificate for each profile
		svcName := key.ResourceName
		if svcName == "" {
			continue
		}
		if ok := createUpdateTLSServer(prof, svcName, sharedApp); ok {
			// Create Certificate only if the corresponding TLSServer is created
			createCertificateDecl(prof, sharedApp)
		} else {
			createUpdateCABundle(prof, caBundleName, sharedApp)
			tlsClient = createTLSClient(prof, svcName, caBundleName, sharedApp)

			skey := SecretKey{
				Name: prof.Name + "-ca",
			}
			if _, ok := customProfiles.Profs[skey]; ok && tlsClient != nil {
				// If a profile exist in customProfiles with key as created above
				// then it indicates that secure-serverssl needs to be added
				tlsClient.ValidateCertificate = true
			}
		}
	}
}

// createUpdateTLSServer creates a new TLSServer instance or updates if one exists already
func createUpdateTLSServer(prof CustomProfile, svcName string, sharedApp as3Application) bool {
	// A TLSServer profile needs to carry both Certificate and Key
	if "" != prof.Cert && "" != prof.Key {
		if sharedApp[svcName] == nil {
			return false
		}
		svc := sharedApp[svcName].(*as3Service)
		tlsServerName := fmt.Sprintf("%s_tls_server", svcName)
		certName := prof.Name

		tlsServer, ok := sharedApp[tlsServerName].(*as3TLSServer)
		if !ok {
			tlsServer = &as3TLSServer{
				Class:        "TLS_Server",
				Certificates: []as3TLSServerCertificates{},
			}

			sharedApp[tlsServerName] = tlsServer
			svc.ServerTLS = tlsServerName
			updateVirtualToHTTPS(svc)
		}

		tlsServer.Certificates = append(
			tlsServer.Certificates,
			as3TLSServerCertificates{
				Certificate: certName,
			},
		)
		return true
	}
	return false
}

func createCertificateDecl(prof CustomProfile, sharedApp as3Application) {
	if "" != prof.Cert && "" != prof.Key {
		cert := &as3Certificate{
			Class:       "Certificate",
			Certificate: prof.Cert,
			PrivateKey:  prof.Key,
			ChainCA:     prof.CAFile,
		}
		sharedApp[prof.Name] = cert
	}
}

func createUpdateCABundle(prof CustomProfile, caBundleName string, sharedApp as3Application) {
	// For TLSClient only Cert (DestinationCACertificate) is given and key is empty string
	if "" != prof.Cert && "" == prof.Key {
		caBundle, ok := sharedApp[caBundleName].(*as3CABundle)

		if !ok {
			caBundle = &as3CABundle{
				Class:  "CA_Bundle",
				Bundle: "",
			}
			sharedApp[caBundleName] = caBundle
		}
		caBundle.Bundle += "\n" + prof.Cert
	}
}

func createTLSClient(
	prof CustomProfile,
	svcName, caBundleName string,
	sharedApp as3Application,
) *as3TLSClient {
	// For TLSClient only Cert (DestinationCACertificate) is given and key is empty string
	if _, ok := sharedApp[svcName]; "" != prof.Cert && "" == prof.Key && ok {
		svc := sharedApp[svcName].(*as3Service)
		tlsClientName := fmt.Sprintf("%s_tls_client", svcName)

		tlsClient := &as3TLSClient{
			Class: "TLS_Client",
			TrustCA: &as3ResourcePointer{
				Use: caBundleName,
			},
		}

		sharedApp[tlsClientName] = tlsClient
		svc.ClientTLS = tlsClientName
		updateVirtualToHTTPS(svc)

		return tlsClient
	}
	return nil
}

//Create health monitor declaration
func createMonitorDecl(cfg *ResourceConfig, sharedApp as3Application) {

	for _, v := range cfg.Monitors {
		monitor := &as3Monitor{}
		monitor.Class = "Monitor"
		monitor.Interval = v.Interval
		monitor.MonitorType = v.Type
		monitor.Timeout = v.Timeout
		val := 0
		monitor.TargetPort = &val
		targetAddressStr := ""
		monitor.TargetAddress = &targetAddressStr
		//Monitor type
		switch v.Type {
		case "http":
			adaptiveFalse := false
			monitor.Adaptive = &adaptiveFalse
			monitor.Dscp = &val
			monitor.Receive = "none"
			if v.Recv != "" {
				monitor.Receive = v.Recv
			}
			monitor.TimeUnitilUp = &val
			monitor.Send = v.Send
		case "https":
			//Todo: For https monitor type
			adaptiveFalse := false
			monitor.Adaptive = &adaptiveFalse
			if v.Recv != "" {
				monitor.Receive = v.Recv
			}
			monitor.Send = v.Send
		case "tcp":
			adaptiveFalse := false
			monitor.Adaptive = &adaptiveFalse
			monitor.Receive = v.Recv
			monitor.Send = v.Send
		}
		sharedApp[v.Name] = monitor
	}

}

// Create AS3 transport Service for CRD
func createTransportServiceDecl(cfg *ResourceConfig, sharedApp as3Application) {
	svc := &as3Service{}

	if cfg.Virtual.Mode == "standard" {
		svc.Class = "Service_TCP"
	} else if cfg.Virtual.Mode == "performance" {
		svc.Class = "Service_L4"
	}
	svc.ProfileL4 = "basic"
	if cfg.Virtual.SNAT == "auto" || cfg.Virtual.SNAT == "none" {
		svc.SNAT = cfg.Virtual.SNAT
	} else {
		svc.SNAT = &as3ResourcePointer{
			BigIP: fmt.Sprintf("%v", cfg.Virtual.SNAT),
		}
	}
	if cfg.Virtual.TranslateServerAddress == true {
		svc.TranslateServerAddress = cfg.Virtual.TranslateServerAddress
	}
	if cfg.Virtual.TranslateServerPort == true {
		svc.TranslateServerPort = cfg.Virtual.TranslateServerPort
	}
	if cfg.Virtual.Source != "" {
		svc.Source = cfg.Virtual.Source
	}
	virtualAddress, port := extractVirtualAddressAndPort(cfg.Virtual.Destination)
	// verify that ip address and port exists.
	if virtualAddress != "" && port != 0 {
		va := append(svc.VirtualAddresses, virtualAddress)
		svc.VirtualAddresses = va
		svc.VirtualPort = port
	}
	svc.Pool = cfg.Virtual.PoolName
	if cfg.Virtual.AllowVLANs != nil {
		for _, vlan := range cfg.Virtual.AllowVLANs {
			vlans := as3ResourcePointer{BigIP: vlan}
			svc.AllowVLANs = append(svc.AllowVLANs, vlans)
		}
	}
	//process irules for crd
	processIrulesForCRD(cfg, svc)
	sharedApp[cfg.Virtual.Name] = svc
}
