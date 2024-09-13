package tapfs

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path/filepath"
)

func Readdir(dir string) ([]JSONOpenData, error) {
	var res []JSONOpenData
	fns, err := filepath.Glob(dir + "/*")
	if err != nil {
		return nil, err
	}

	for _, fn := range fns {
		data, err := ioutil.ReadFile(fn)
		if err != nil {
			return nil, err
		}

		var od JSONOpenData
		if err := json.Unmarshal(data, &od); err != nil {
			return nil, fmt.Errorf("Unmarshal(%s): %v", fn, err)
		}

		res = append(res, od)
	}
	return res, nil
}
