package feedservice

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/greenbone/eulabeia/connection"
	"github.com/greenbone/eulabeia/feedservice/handler"
	"github.com/greenbone/eulabeia/feedservice/redis"
	"github.com/greenbone/eulabeia/messages/cmds"
	"github.com/greenbone/eulabeia/messages/info"
	"github.com/greenbone/eulabeia/models"
)

type DBConnecion interface {
	Close() error
	GetList(db int, key string, start int, end int) ([]string, error)
	GetKeys(db int, filter string) ([]string, error)
}

// feed is the struct representing the feedservice
type feed struct {
	mqtt    connection.PubSub
	context string
	rc      DBConnecion
	id      string
}

// getSeverity filters the necessary infomation from the tags of a nvti to
// create a models.SeverityType
func getSeverity(tags map[string]string) models.SeverityType {
	var severetyVector string
	var severetyVersion string
	var severetyDate string

	if v, ok := tags["severety_vector"]; ok {
		severetyVector = v
	} else {
		severetyVector = tags["cvss_base_vector"]
	}
	if strings.Contains(severetyVector, "CVSS:3") {
		severetyVersion = "cvss_base_v3"
	} else {
		severetyVersion = "cvss_base_v2"
	}
	if v, ok := tags["severety_date"]; ok {
		severetyDate = v
	} else {
		severetyDate = tags["creation_date"]
	}

	return models.SeverityType{
		Vector:  severetyVector,
		Version: severetyVersion,
		Date:    severetyDate,
		Origin:  tags["severity_origin"],
	}
}

// getRefs expects a comma separated list of cves, bids and xrefs. The function
// splitts them and put them into a list of models.RefType
func getRefs(cves string, bids string, xrefs string) []models.RefType {
	var cveSlice []string
	var bidSlice []string
	var xrefSlice []string

	var l int

	if cves != "" {
		cveSlice = strings.Split(cves, ", ")
		l += len(cveSlice)
	}

	if bids != "" {
		bidSlice = strings.Split(bids, ", ")
		l += len(bidSlice)
	}

	if xrefs != "" {
		xrefSlice = strings.Split(xrefs, ", ")
		l += len(xrefSlice)
	}

	ret := make([]models.RefType, l)
	i := 0
	for _, v := range cveSlice {
		ret[i] = models.RefType{
			Type: "CVE",
			ID:   v,
		}
		i++
	}
	for _, v := range bidSlice {
		ret[i] = models.RefType{
			Type: "BID",
			ID:   v,
		}
		i++
	}
	for _, v := range xrefSlice {
		xref := strings.SplitN(v, ":", 2)
		if len(xref) != 2 {
			continue
		}
		ret[i] = models.RefType{
			Type: xref[0],
			ID:   xref[1],
		}
		i++
	}

	return ret
}

// getNvtPrefs expects an oid corresponding to a nvt. The function parses the
// preferences of a nvt into a list of models.VTParamType
func (f *feed) getNvtPrefs(oid string) []models.VTParamType {
	key := fmt.Sprintf("oid:%s:prefs", oid)
	prefs, err := f.rc.GetList(1, key, 0, -1)
	if err != nil {
		return nil
	}

	ret := make([]models.VTParamType, len(prefs))
	for i, v := range prefs {
		pref := strings.Split(v, "|||")
		id, err := strconv.Atoi(pref[0])
		if err != nil {
			return nil
		}
		def := ""
		if len(pref) > 3 {
			def = pref[3]
		}
		ret[i] = models.VTParamType{
			ID:   id,
			Name: pref[1],
			// value cannot be set via nasl plugin
			Value: "",
			Type:  pref[2],
			// description cannot be set via nasl plugin
			Description:  "",
			DefaultValue: def,
		}
	}
	return ret

}

// GetVt expects a single OIDs and returns all metadata of the corresponding VT.
func (f *feed) GetVT(msg cmds.Get) (models.VT, *info.Failure, error) {
	log.Printf("In get vt for %s", msg.ID)
	pref, err := f.rc.GetList(1, fmt.Sprintf("nvt:%s", msg.ID), 0, -1)
	if err != nil {
		return models.VT{}, nil, err
	}
	if len(pref) == 0 {
		return models.VT{}, info.GetFailureResponse(msg.Message, "vt", msg.ID), nil
	}
	log.Printf("Got %d vts", len(pref))

	dependecies := strings.Split(pref[redis.NVT_DEPENDENCIES_POS], ", ")
	allTags := strings.Split(pref[redis.NVT_TAGS_POS], "|")
	tags := make(map[string]string)

	for _, v := range allTags {
		tag := strings.SplitN(v, "=", 2)
		tags[tag[0]] = tag[1]
	}
	refs := getRefs(pref[redis.NVT_CVES_POS], pref[redis.NVT_BIDS_POS], pref[redis.NVT_XREFS_POS])

	vt := models.VT{
		OID:                msg.ID,
		Name:               pref[redis.NVT_NAME_POS],
		FileName:           pref[redis.NVT_FILENAME_POS],
		RequiredKeys:       pref[redis.NVT_REQUIRED_KEYS_POS],
		MandatoryKeys:      pref[redis.NVT_MANDATORY_KEYS_POS],
		ExcludedKeys:       pref[redis.NVT_EXCLUDED_KEYS_POS],
		RequiredPorts:      pref[redis.NVT_REQUIRED_PORTS_POS],
		RequiredUDPPorts:   pref[redis.NVT_REQUIRED_UDP_PORTS_POS],
		Category:           pref[redis.NVT_CATEGORY_POS],
		Family:             pref[redis.NVT_FAMILY_POS],
		VTCreationTime:     tags["creation_date"],
		VTModificationTime: tags["last_modification"],
		Summary:            tags["summary"],
		Solution:           tags["solution"],
		SolutionType:       tags["solution_type"],
		SolutionMethod:     tags["solution_method"],
		Impact:             tags["impact"],
		Insight:            tags["insight"],
		Affected:           tags["affected"],
		Vuldetect:          tags["vuldetect"],
		QoDType:            tags["qod_type"],
		QoDValue:           tags["qod"],
		References:         refs,
		VTParameters:       f.getNvtPrefs(msg.ID),
		VTDependencies:     dependecies,
		Severity:           getSeverity(tags),
	}

	return vt, nil, err
}

// GetVTs expects a List of VTFilter and returns a list of oids which match the given filter.
func (f *feed) ResolveFilter(filter []models.VTFilter) ([]string, error) {
	ret := make([]string, 0)

	if len(filter) == 0 {
		return nil, fmt.Errorf("empty or missing filter")
	}

	vtOIDs, err := f.rc.GetKeys(1, "nvt:*")
	if err != nil {
		return nil, err
	}

	var contains bool
	for _, nvtOID := range vtOIDs {
		oid := strings.TrimPrefix(nvtOID, "nvt:")
		vt, err := f.rc.GetList(1, nvtOID, redis.NVT_FILENAME_POS, redis.NVT_NAME_POS)
		if err != nil {
			continue
		}
		contains = false
		for _, v := range filter {
			switch v.Key {
			case "family":
				contains = strings.Contains(vt[redis.NVT_FAMILY_POS], v.Value)
			case "category":
				contains = strings.Contains(vt[redis.NVT_CATEGORY_POS], v.Value)
			case "tag":
				contains = strings.Contains(vt[redis.NVT_TAGS_POS], v.Value)
			case "cve":
				contains = strings.Contains(vt[redis.NVT_CVES_POS], v.Value)
			case "name":
				contains = strings.Contains(vt[redis.NVT_NAME_POS], v.Value)
			case "filename":
				contains = strings.Contains(vt[redis.NVT_FILENAME_POS], v.Value)
			case "bid":
				contains = strings.Contains(vt[redis.NVT_BIDS_POS], v.Value)
			}
			if contains {
				ret = append(ret, oid)
				continue
			}
		}
	}

	return ret, nil

}

// Start starts the feed service
func (f *feed) Start() {
	fmt.Printf("%s/vt/cmd/%s\n", f.context, f.id)
	// MQTT Subscription Map
	f.mqtt.Subscribe(map[string]connection.OnMessage{
		fmt.Sprintf("%s/vt/cmd/%s", f.context, f.id): handler.FeedHandler{
			GetVT:         f.GetVT,
			ResolveFilter: f.ResolveFilter,
			Context:       f.context,
		},
	})
}

// Close ends the feed service
func (f *feed) Close() error {
	return f.rc.Close()
}

// NewScheduler creates a new scheduler
func NewFeed(mqtt connection.PubSub, context string, id string, redisPath string) *feed {
	return &feed{
		mqtt:    mqtt,
		context: context,
		rc:      redis.NewRedisConnection("unix", redisPath),
		id:      id,
	}
}
