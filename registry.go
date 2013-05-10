package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/dotcloud/docker/auth"
	"github.com/shin-/cookiejar"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

// Build an Image object from raw json data
func NewImgJson(src []byte) (*Image, error) {
	ret := &Image{}

	Debugf("Json string: {%s}\n", src)
	// FIXME: Is there a cleaner way to "purify" the input json?
	if err := json.Unmarshal(src, ret); err != nil {
		return nil, err
	}
	return ret, nil
}

func doWithCookies(c *http.Client, req *http.Request) (*http.Response, error) {
	for _, cookie := range c.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	return c.Do(req)
}

// Retrieve the history of a given image from the Registry.
// Return a list of the parent's json (requested image included)
func (graph *Graph) getRemoteHistory(imgId, registry string, token []string) ([]string, error) {
	client := graph.getHttpClient()

	req, err := http.NewRequest("GET", registry+"/images/"+imgId+"/ancestry", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+strings.Join(token, ", "))
	res, err := client.Do(req)
	if err != nil || res.StatusCode != 200 {
		if res != nil {
			return nil, fmt.Errorf("Internal server error: %d trying to fetch remote history for %s", res.StatusCode, imgId)
		}
		return nil, err
	}
	defer res.Body.Close()

	jsonString, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("Error while reading the http response: %s\n", err)
	}

	Debugf("Ancestry: %s", jsonString)
	history := new([]string)
	if err := json.Unmarshal(jsonString, history); err != nil {
		return nil, err
	}
	return *history, nil
}

func (graph *Graph) getHttpClient() *http.Client {
	if graph.httpClient == nil {
		graph.httpClient = &http.Client{}
		graph.httpClient.Jar = cookiejar.NewCookieJar()
	}
	return graph.httpClient
}

// Check if an image exists in the Registry
func (graph *Graph) LookupRemoteImage(imgId, registry string, authConfig *auth.AuthConfig) bool {
	rt := &http.Transport{Proxy: http.ProxyFromEnvironment}

	req, err := http.NewRequest("GET", registry+"/images/"+imgId+"/json", nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth(authConfig.Username, authConfig.Password)
	res, err := rt.RoundTrip(req)
	return err == nil && res.StatusCode == 307
}

func (graph *Graph) getImagesInRepository(repository string, authConfig *auth.AuthConfig) ([]map[string]string, error) {
	u := auth.IndexServerAddress() + "/repositories/" + repository + "/images"
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	if authConfig != nil && len(authConfig.Username) > 0 {
		req.SetBasicAuth(authConfig.Username, authConfig.Password)
	}
	res, err := graph.getHttpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// Repository doesn't exist yet
	if res.StatusCode == 404 {
		return nil, nil
	}

	jsonData, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	imageList := []map[string]string{}

	err = json.Unmarshal(jsonData, &imageList)
	if err != nil {
		Debugf("Body: %s (%s)\n", res.Body, u)
		return nil, err
	}

	return imageList, nil
}

// Retrieve an image from the Registry.
// Returns the Image object as well as the layer as an Archive (io.Reader)
func (graph *Graph) getRemoteImage(stdout io.Writer, imgId, registry string, token []string) (*Image, Archive, error) {
	client := graph.getHttpClient()

	fmt.Fprintf(stdout, "Pulling %s metadata\r\n", imgId)
	// Get the Json
	req, err := http.NewRequest("GET", registry+"/images/"+imgId+"/json", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to download json: %s", err)
	}
	req.Header.Set("Authorization", "Token "+strings.Join(token, ", "))
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to download json: %s", err)
	}
	if res.StatusCode != 200 {
		return nil, nil, fmt.Errorf("HTTP code %d", res.StatusCode)
	}
	defer res.Body.Close()

	jsonString, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to download json: %s", err)
	}

	img, err := NewImgJson(jsonString)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to parse json: %s", err)
	}
	img.Id = imgId

	// Get the layer
	fmt.Fprintf(stdout, "Pulling %s fs layer\r\n", imgId)
	req, err = http.NewRequest("GET", registry+"/images/"+imgId+"/layer", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("Error while getting from the server: %s\n", err)
	}
	req.Header.Set("Authorization", "Token "+strings.Join(token, ", "))
	res, err = client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	return img, ProgressReader(res.Body, int(res.ContentLength), stdout, "Downloading %v/%v (%v)"), nil
}

func (graph *Graph) getRemoteTags(stdout io.Writer, registries []string, repository string, token []string) (map[string]string, error) {
	client := graph.getHttpClient()
	if strings.Count(repository, "/") == 0 {
		// This will be removed once the Registry supports auto-resolution on
		// the "library" namespace
		repository = "library/" + repository
	}
	for _, host := range registries {
		endpoint := fmt.Sprintf("https://%s/v1/repositories/%s/tags", host, repository)
		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Token "+strings.Join(token, ", "))
		res, err := client.Do(req)
		defer res.Body.Close()
		Debugf("Got status code %d from %s", res.StatusCode, endpoint)
		if err != nil || (res.StatusCode != 200 && res.StatusCode != 404) {
			continue
		} else if res.StatusCode == 404 {
			return nil, fmt.Errorf("Repository not found")
		}

		result := make(map[string]string)

		rawJson, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(rawJson, &result); err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, fmt.Errorf("Could not reach any registry endpoint")
}

func (graph *Graph) getImageForTag(stdout io.Writer, tag, remote, registry string, token []string) (string, error) {
	client := graph.getHttpClient()

	if !strings.Contains(remote, "/") {
		remote = "library/" + remote
	}

	registryEndpoint := "https://" + registry + "/v1"
	repositoryTarget := registryEndpoint + "/repositories/" + remote + "/tags/" + tag

	req, err := http.NewRequest("GET", repositoryTarget, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Token "+strings.Join(token, ", "))
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Error while retrieving repository info: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == 403 {
		return "", fmt.Errorf("You aren't authorized to access this resource")
	} else if res.StatusCode != 200 {
		return "", fmt.Errorf("HTTP code: %d", res.StatusCode)
	}

	var imgId string
	rawJson, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	if err = json.Unmarshal(rawJson, &imgId); err != nil {
		return "", err
	}
	return imgId, nil
}

func (graph *Graph) PullImage(stdout io.Writer, imgId, registry string, token []string) error {
	history, err := graph.getRemoteHistory(imgId, registry, token)
	if err != nil {
		return err
	}
	// FIXME: Try to stream the images?
	// FIXME: Launch the getRemoteImage() in goroutines
	for _, id := range history {
		if !graph.Exists(id) {
			img, layer, err := graph.getRemoteImage(stdout, id, registry, token)
			if err != nil {
				// FIXME: Keep goging in case of error?
				return err
			}
			if err = graph.Register(layer, false, img); err != nil {
				return err
			}
		}
	}
	return nil
}

func (graph *Graph) PullRepository(stdout io.Writer, remote, askedTag string, repositories *TagStore, authConfig *auth.AuthConfig) error {
	client := graph.getHttpClient()

	fmt.Fprintf(stdout, "Pulling repository %s from %s\r\n", remote, auth.IndexServerAddress())
	repositoryTarget := auth.IndexServerAddress() + "/repositories/" + remote + "/images"

	req, err := http.NewRequest("GET", repositoryTarget, nil)
	if err != nil {
		return err
	}
	if authConfig != nil && len(authConfig.Username) > 0 {
		req.SetBasicAuth(authConfig.Username, authConfig.Password)
	}
	req.Header.Set("X-Docker-Token", "true")

	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == 401 {
		return fmt.Errorf("Please login first (HTTP code %d)", res.StatusCode)
	}
	// TODO: Right now we're ignoring checksums in the response body.
	// In the future, we need to use them to check image validity.
	if res.StatusCode != 200 {
		return fmt.Errorf("HTTP code: %d", res.StatusCode)
	}

	var token, endpoints []string
	if res.Header.Get("X-Docker-Token") != "" {
		token = res.Header["X-Docker-Token"]
	}
	if res.Header.Get("X-Docker-Endpoints") != "" {
		endpoints = res.Header["X-Docker-Endpoints"]
	} else {
		return fmt.Errorf("Index response didn't contain any endpoints")
	}

	checksumsJson, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	// Reload the json file to make sure not to overwrite faster sums
	err = func() error {
		localChecksums := make(map[string]string)
		remoteChecksums := []ImgListJson{}
		checksumDictPth := path.Join(graph.Root, "..", "checksums")

		if err := json.Unmarshal(checksumsJson, &remoteChecksums); err != nil {
			return err
		}

		graph.lockSumFile.Lock()
		defer graph.lockSumFile.Unlock()

		if checksumDict, err := ioutil.ReadFile(checksumDictPth); err == nil {
			if err := json.Unmarshal(checksumDict, &localChecksums); err != nil {
				return err
			}
		}

		for _, elem := range remoteChecksums {
			localChecksums[elem.Id] = elem.Checksum
		}

		checksumsJson, err = json.Marshal(localChecksums)
		if err != nil {
			return err
		}
		if err := ioutil.WriteFile(checksumDictPth, checksumsJson, 0600); err != nil {
			return err
		}
		return nil
	}()
	if err != nil {
		return err
	}

	var tagsList map[string]string
	if askedTag == "" {
		tagsList, err = graph.getRemoteTags(stdout, endpoints, remote, token)
		if err != nil {
			return err
		}
	} else {
		tagsList = map[string]string{askedTag: ""}
	}

	for askedTag, imgId := range tagsList {
		fmt.Fprintf(stdout, "Resolving tag \"%s:%s\" from %s\n", remote, askedTag, endpoints)
		success := false
		for _, registry := range endpoints {
			if imgId == "" {
				imgId, err = graph.getImageForTag(stdout, askedTag, remote, registry, token)
				if err != nil {
					fmt.Fprintf(stdout, "Error while retrieving image for tag: %v (%v) ; "+
						"checking next endpoint", askedTag, err)
					continue
				}
			}

			if err := graph.PullImage(stdout, imgId, "https://"+registry+"/v1", token); err != nil {
				return err
			}

			if err = repositories.Set(remote, askedTag, imgId, true); err != nil {
				return err
			}
			success = true
		}

		if !success {
			return fmt.Errorf("Could not find repository on any of the indexed registries.")
		}
	}

	if err = repositories.Save(); err != nil {
		return err
	}

	return nil
}

// Push a local image to the registry
func (graph *Graph) PushImage(stdout io.Writer, img *Image, registry string, token []string) error {
	registry = "https://" + registry + "/v1"

	client := graph.getHttpClient()
	jsonRaw, err := ioutil.ReadFile(path.Join(graph.Root, img.Id, "json"))
	if err != nil {
		return fmt.Errorf("Error while retreiving the path for {%s}: %s", img.Id, err)
	}

	fmt.Fprintf(stdout, "Pushing %s metadata\r\n", img.Id)

	// FIXME: try json with UTF8
	jsonData := strings.NewReader(string(jsonRaw))
	req, err := http.NewRequest("PUT", registry+"/images/"+img.Id+"/json", jsonData)
	if err != nil {
		return err
	}
	req.Header.Add("Content-type", "application/json")
	req.Header.Set("Authorization", "Token "+strings.Join(token, ","))

	checksum, err := img.Checksum()
	if err != nil {
		return fmt.Errorf("Error while retrieving checksum for %s: %v", img.Id, err)
	}
	req.Header.Set("X-Docker-Checksum", checksum)
	Debugf("Setting checksum for %s: %s", img.ShortId(), checksum)
	res, err := doWithCookies(client, req)
	if err != nil {
		return fmt.Errorf("Failed to upload metadata: %s", err)
	}
	defer res.Body.Close()
	if len(res.Cookies()) > 0 {
		client.Jar.SetCookies(req.URL, res.Cookies())
	}
	if res.StatusCode != 200 {
		errBody, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return fmt.Errorf("HTTP code %d while uploading metadata and error when"+
				" trying to parse response body: %v", res.StatusCode, err)
		}
		var jsonBody map[string]string
		if err := json.Unmarshal(errBody, &jsonBody); err != nil {
			errBody = []byte(err.Error())
		} else if jsonBody["error"] == "Image already exists" {
			fmt.Fprintf(stdout, "Image %v already uploaded ; skipping\n", img.Id)
			return nil
		}
		return fmt.Errorf("HTTP code %d while uploading metadata: %s", res.StatusCode, errBody)
	}

	fmt.Fprintf(stdout, "Pushing %s fs layer\r\n", img.Id)
	root, err := img.root()
	if err != nil {
		return err
	}

	var layerData *TempArchive
	// If the archive exists, use it
	file, err := os.Open(layerArchivePath(root))
	if err != nil {
		if os.IsNotExist(err) {
			// If the archive does not exist, create one from the layer
			layerData, err = graph.TempLayerArchive(img.Id, Xz, stdout)
			if err != nil {
				return fmt.Errorf("Failed to generate layer archive: %s", err)
			}
		} else {
			return err
		}
	} else {
		defer file.Close()
		st, err := file.Stat()
		if err != nil {
			return err
		}
		layerData = &TempArchive{file, st.Size()}
	}

	req3, err := http.NewRequest("PUT", registry+"/images/"+img.Id+"/layer",
		ProgressReader(layerData, int(layerData.Size), stdout, ""))
	if err != nil {
		return err
	}

	req3.ContentLength = -1
	req3.TransferEncoding = []string{"chunked"}
	req3.Header.Set("Authorization", "Token "+strings.Join(token, ","))
	res3, err := doWithCookies(client, req3)
	if err != nil {
		return fmt.Errorf("Failed to upload layer: %s", err)
	}
	defer res3.Body.Close()

	if res3.StatusCode != 200 {
		errBody, err := ioutil.ReadAll(res3.Body)
		if err != nil {
			return fmt.Errorf("HTTP code %d while uploading metadata and error when"+
				" trying to parse response body: %v", res.StatusCode, err)
		}
		return fmt.Errorf("Received HTTP code %d while uploading layer: %s", res3.StatusCode, errBody)
	}
	return nil
}

// push a tag on the registry.
// Remote has the format '<user>/<repo>
func (graph *Graph) pushTag(remote, revision, tag, registry string, token []string) error {
	// "jsonify" the string
	revision = "\"" + revision + "\""
	registry = "https://" + registry + "/v1"

	Debugf("Pushing tags for rev [%s] on {%s}\n", revision, registry+"/users/"+remote+"/"+tag)

	client := graph.getHttpClient()
	req, err := http.NewRequest("PUT", registry+"/repositories/"+remote+"/tags/"+tag, strings.NewReader(revision))
	if err != nil {
		return err
	}
	req.Header.Add("Content-type", "application/json")
	req.Header.Set("Authorization", "Token "+strings.Join(token, ","))
	req.ContentLength = int64(len(revision))
	res, err := doWithCookies(client, req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 201 {
		return fmt.Errorf("Internal server error: %d trying to push tag %s on %s", res.StatusCode, tag, remote)
	}
	return nil
}

// FIXME: this should really be PushTag
func (graph *Graph) pushPrimitive(stdout io.Writer, remote, tag, imgId, registry string, token []string) error {
	// Check if the local impage exists
	img, err := graph.Get(imgId)
	if err != nil {
		fmt.Fprintf(stdout, "Skipping tag %s:%s: %s does not exist\r\n", remote, tag, imgId)
		return nil
	}
	fmt.Fprintf(stdout, "Pushing image %s:%s\r\n", remote, tag)
	// Push the image
	if err = graph.PushImage(stdout, img, registry, token); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Registering tag %s:%s\r\n", remote, tag)
	// And then the tag
	if err = graph.pushTag(remote, imgId, tag, registry, token); err != nil {
		return err
	}
	return nil
}

// Retrieve the checksum of an image
// Priority:
// - Check on the stored checksums
// - Check if the archive exists, if it does not, ask the registry
// - If the archive does exists, process the checksum from it
// - If the archive does not exists and not found on registry, process checksum from layer
func (graph *Graph) getChecksum(imageId string) (string, error) {
	// FIXME: Use in-memory map instead of reading the file each time
	if sums, err := graph.getStoredChecksums(); err != nil {
		return "", err
	} else if checksum, exists := sums[imageId]; exists {
		return checksum, nil
	}

	img, err := graph.Get(imageId)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(layerArchivePath(graph.imageRoot(imageId))); err != nil {
		if os.IsNotExist(err) {
			// TODO: Ask the registry for the checksum
			//       As the archive is not there, it is supposed to come from a pull.
		} else {
			return "", err
		}
	}

	checksum, err := img.Checksum()
	if err != nil {
		return "", err
	}
	return checksum, nil
}

type ImgListJson struct {
	Id       string `json:"id"`
	Checksum string `json:"checksum,omitempty"`
	tag      string
}

// Push a repository to the registry.
// Remote has the format '<user>/<repo>
func (graph *Graph) PushRepository(stdout io.Writer, remote string, localRepo Repository, authConfig *auth.AuthConfig) error {
	client := graph.getHttpClient()
	// FIXME: Do not reset the cookie each time? (need to reset it in case updating latest of a repo and repushing)
	client.Jar = cookiejar.NewCookieJar()
	var imgList []*ImgListJson

	fmt.Fprintf(stdout, "Processing checksums\n")
	imageSet := make(map[string]struct{})

	for tag, id := range localRepo {
		img, err := graph.Get(id)
		if err != nil {
			return err
		}
		img.WalkHistory(func(img *Image) error {
			if _, exists := imageSet[img.Id]; exists {
				return nil
			}
			imageSet[img.Id] = struct{}{}
			checksum, err := graph.getChecksum(img.Id)
			if err != nil {
				return err
			}
			imgList = append([]*ImgListJson{{
				Id:       img.Id,
				Checksum: checksum,
				tag:      tag,
			}}, imgList...)
			return nil
		})
	}

	imgListJson, err := json.Marshal(imgList)
	if err != nil {
		return err
	}

	Debugf("json sent: %s\n", imgListJson)

	fmt.Fprintf(stdout, "Sending image list\n")
	req, err := http.NewRequest("PUT", auth.IndexServerAddress()+"/repositories/"+remote+"/", bytes.NewReader(imgListJson))
	if err != nil {
		return err
	}
	req.SetBasicAuth(authConfig.Username, authConfig.Password)
	req.ContentLength = int64(len(imgListJson))
	req.Header.Set("X-Docker-Token", "true")

	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	for res.StatusCode >= 300 && res.StatusCode < 400 {
		Debugf("Redirected to %s\n", res.Header.Get("Location"))
		req, err = http.NewRequest("PUT", res.Header.Get("Location"), bytes.NewReader(imgListJson))
		if err != nil {
			return err
		}
		req.SetBasicAuth(authConfig.Username, authConfig.Password)
		req.ContentLength = int64(len(imgListJson))
		req.Header.Set("X-Docker-Token", "true")

		res, err = client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
	}

	if res.StatusCode != 200 && res.StatusCode != 201 {
		errBody, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return err
		}
		return fmt.Errorf("Error: Status %d trying to push repository %s: %s", res.StatusCode, remote, errBody)
	}

	var token, endpoints []string
	if res.Header.Get("X-Docker-Token") != "" {
		token = res.Header["X-Docker-Token"]
		Debugf("Auth token: %v", token)
	} else {
		return fmt.Errorf("Index response didn't contain an access token")
	}
	if res.Header.Get("X-Docker-Endpoints") != "" {
		endpoints = res.Header["X-Docker-Endpoints"]
	} else {
		return fmt.Errorf("Index response didn't contain any endpoints")
	}

	// FIXME: Send only needed images
	for _, registry := range endpoints {
		fmt.Fprintf(stdout, "Pushing repository %s to %s (%d tags)\r\n", remote, registry, len(localRepo))
		// For each image within the repo, push them
		for _, elem := range imgList {
			if err := graph.pushPrimitive(stdout, remote, elem.tag, elem.Id, registry, token); err != nil {
				// FIXME: Continue on error?
				return err
			}
		}
	}

	req2, err := http.NewRequest("PUT", auth.IndexServerAddress()+"/repositories/"+remote+"/images", bytes.NewReader(imgListJson))
	if err != nil {
		return err
	}
	req2.SetBasicAuth(authConfig.Username, authConfig.Password)
	req2.Header["X-Docker-Endpoints"] = endpoints
	req2.ContentLength = int64(len(imgListJson))
	res2, err := client.Do(req2)
	if err != nil {
		return err
	}
	defer res2.Body.Close()
	if res2.StatusCode != 204 {
		if errBody, err := ioutil.ReadAll(res2.Body); err != nil {
			return err
		} else {
			return fmt.Errorf("Error: Status %d trying to push checksums %s: %s", res2.StatusCode, remote, errBody)
		}
	}

	return nil
}

type SearchResults struct {
	Query      string              `json:"query"`
	NumResults int                 `json:"num_results"`
	Results    []map[string]string `json:"results"`
}

func (graph *Graph) SearchRepositories(stdout io.Writer, term string) (*SearchResults, error) {
	client := graph.getHttpClient()
	u := auth.IndexServerAddress() + "/search?q=" + url.QueryEscape(term)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Unexepected status code %d", res.StatusCode)
	}
	rawData, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	result := new(SearchResults)
	err = json.Unmarshal(rawData, result)
	return result, err
}
