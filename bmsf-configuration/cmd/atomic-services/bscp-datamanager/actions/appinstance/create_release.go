/*
Tencent is pleased to support the open source community by making Blueking Container Service available.
Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
Licensed under the MIT License (the "License"); you may not use this file except
in compliance with the License. You may obtain a copy of the License at
http://opensource.org/licenses/MIT
Unless required by applicable law or agreed to in writing, software distributed under
the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
either express or implied. See the License for the specific language governing permissions and
limitations under the License.
*/

package appinstance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/spf13/viper"

	"bk-bscp/cmd/atomic-services/bscp-datamanager/modules/metrics"
	"bk-bscp/internal/database"
	"bk-bscp/internal/dbsharding"
	pbcommon "bk-bscp/internal/protocol/common"
	pb "bk-bscp/internal/protocol/datamanager"
	"bk-bscp/internal/strategy"
	"bk-bscp/pkg/common"
	"bk-bscp/pkg/logger"
)

// CreateReleaseAction is appinstance release create action object.
type CreateReleaseAction struct {
	ctx   context.Context
	viper *viper.Viper
	smgr  *dbsharding.ShardingManager

	collector *metrics.Collector

	req  *pb.CreateAppInstanceReleaseReq
	resp *pb.CreateAppInstanceReleaseResp

	sd *dbsharding.ShardingDB

	appInstance database.AppInstance
}

// NewCreateReleaseAction creates new CreateReleaseAction.
func NewCreateReleaseAction(ctx context.Context, viper *viper.Viper,
	smgr *dbsharding.ShardingManager, collector *metrics.Collector,
	req *pb.CreateAppInstanceReleaseReq, resp *pb.CreateAppInstanceReleaseResp) *CreateReleaseAction {

	action := &CreateReleaseAction{ctx: ctx, viper: viper, smgr: smgr, collector: collector, req: req, resp: resp}

	action.resp.Seq = req.Seq
	action.resp.Code = pbcommon.ErrCode_E_OK
	action.resp.Message = "OK"

	return action
}

// Err setup error code message in response and return the error.
func (act *CreateReleaseAction) Err(errCode pbcommon.ErrCode, errMsg string) error {
	act.resp.Code = errCode
	act.resp.Message = errMsg
	return errors.New(errMsg)
}

// Input handles the input messages.
func (act *CreateReleaseAction) Input() error {
	if err := act.verify(); err != nil {
		return act.Err(pbcommon.ErrCode_E_DM_PARAMS_INVALID, err.Error())
	}
	return nil
}

// Output handles the output messages.
func (act *CreateReleaseAction) Output() error {
	// do nothing.
	return nil
}

func (act *CreateReleaseAction) verify() error {
	var err error

	if err = common.ValidateString("biz_id", act.req.BizId,
		database.BSCPNOTEMPTY, database.BSCPIDLENLIMIT); err != nil {
		return err
	}
	if err = common.ValidateString("app_id", act.req.AppId,
		database.BSCPNOTEMPTY, database.BSCPIDLENLIMIT); err != nil {
		return err
	}
	if err = common.ValidateString("cloud_id", act.req.CloudId,
		database.BSCPNOTEMPTY, database.BSCPNAMELENLIMIT); err != nil {
		return err
	}
	if err = common.ValidateString("ip", act.req.Ip,
		database.BSCPNOTEMPTY, database.BSCPNORMALSTRLENLIMIT); err != nil {
		return err
	}
	if err = common.ValidateString("labels", act.req.Labels,
		database.BSCPEMPTY, database.BSCPLABELSSIZELIMIT); err != nil {
		return err
	}
	if len(act.req.Labels) == 0 {
		act.req.Labels = strategy.EmptySidecarLabels
	}
	if act.req.Labels != strategy.EmptySidecarLabels {
		labels := strategy.SidecarLabels{}
		if err := json.Unmarshal([]byte(act.req.Labels), &labels); err != nil {
			return fmt.Errorf("invalid input data, labels[%+v], %+v", act.req.Labels, err)
		}
	}

	act.req.Path = filepath.Clean(act.req.Path)
	if err = common.ValidateString("path", act.req.Path,
		database.BSCPNOTEMPTY, database.BSCPCFGFPATHLENLIMIT); err != nil {
		return err
	}
	if err = common.ValidateInt("infos", len(act.req.Infos),
		database.BSCPNOTEMPTY, math.MaxInt32); err != nil {
		return err
	}

	return nil
}

func (act *CreateReleaseAction) queryAppInstance() (pbcommon.ErrCode, string) {
	err := act.sd.DB().
		Where(&database.AppInstance{
			BizID:   act.req.BizId,
			AppID:   act.req.AppId,
			CloudID: act.req.CloudId,
			IP:      act.req.Ip,
			Path:    act.req.Path,
		}).
		Last(&act.appInstance).Error

	// not found.
	if err == dbsharding.RECORDNOTFOUND {
		return pbcommon.ErrCode_E_DM_NOT_FOUND, "appinstance non-exist."
	}
	if err != nil {
		return pbcommon.ErrCode_E_DM_DB_EXEC_ERR, err.Error()
	}
	return pbcommon.ErrCode_E_OK, ""
}

func (act *CreateReleaseAction) createAppInstanceReleaseEffect(info *pbcommon.ReportInfo) (pbcommon.ErrCode, string) {
	effectTime, err := time.ParseInLocation("2006-01-02 15:04:05", info.EffectTime, time.Local)
	if err != nil {
		logger.Warn("CreateAppInstanceRelease[%s]| invalid EffectTime format, %+v", act.req.Seq, err)
		act.collector.StatAppInstanceRelease(false)
		return pbcommon.ErrCode_E_DM_SYSTEM_UNKNOWN, "invalid EffectTime format"
	}

	appInstanceRelease := database.AppInstanceRelease{
		InstanceID: act.appInstance.ID,
		BizID:      act.req.BizId,
		AppID:      act.req.AppId,
		CfgID:      info.CfgId,
		ReleaseID:  info.ReleaseId,
		EffectTime: &effectTime,
		EffectCode: info.EffectCode,
		EffectMsg:  info.EffectMsg,
	}
	if len(info.EffectMsg) > database.BSCPEFFECTRELOADERRLENLIMIT {
		// maybe a large error message.
		appInstanceRelease.EffectMsg = info.EffectMsg[:database.BSCPEFFECTRELOADERRLENLIMIT]
	}

	err = act.sd.DB().
		Where(database.AppInstanceRelease{
			InstanceID: act.appInstance.ID,
			CfgID:      info.CfgId,
			ReleaseID:  info.ReleaseId,
		}).
		Assign(appInstanceRelease).
		FirstOrCreate(&appInstanceRelease).Error

	if err != nil {
		e, ok := err.(*mysql.MySQLError)
		if !ok {
			logger.Warn("CreateAppInstanceRelease[%s]| create instance release effect record, %+v", act.req.Seq, err)
			act.collector.StatAppInstanceRelease(false)
			return pbcommon.ErrCode_E_DM_SYSTEM_UNKNOWN, err.Error()
		}

		if e.Number != 1062 {
			logger.Warn("CreateAppInstanceRelease[%s]| create instance release effect record, %+v", act.req.Seq, err)
			act.collector.StatAppInstanceRelease(false)
			return pbcommon.ErrCode_E_DM_SYSTEM_UNKNOWN, err.Error()
		}

		tryErr := act.sd.DB().
			Where(database.AppInstanceRelease{
				InstanceID: act.appInstance.ID,
				CfgID:      info.CfgId,
				ReleaseID:  info.ReleaseId,
			}).
			Assign(appInstanceRelease).
			FirstOrCreate(&appInstanceRelease).Error

		if tryErr != nil {
			logger.Warn("CreateAppInstanceRelease[%s]| create instance release effect record, try again, %+v",
				act.req.Seq, tryErr)
			act.collector.StatAppInstanceRelease(false)
			return pbcommon.ErrCode_E_DM_SYSTEM_UNKNOWN, err.Error()
		}
	}

	logger.V(4).Infof("CreateAppInstanceRelease[%s]| create app instance release effect record success, %+v",
		act.req.Seq, appInstanceRelease)
	act.collector.StatAppInstanceRelease(true)

	return pbcommon.ErrCode_E_OK, ""
}

func (act *CreateReleaseAction) queryCfgidByReleae(releaseID string) (string, error) {
	var st database.Release

	err := act.sd.DB().
		Where(&database.Release{BizID: act.req.BizId, ReleaseID: releaseID}).
		Last(&st).Error

	// not found.
	if err == dbsharding.RECORDNOTFOUND {
		return "", errors.New("release non-exist")
	}
	if err != nil {
		return "", err
	}
	return st.CfgID, nil
}

func (act *CreateReleaseAction) queryIDsByMultiReleae(multiReleaseID string) ([]string, []string, error) {
	var sts []database.Release

	err := act.sd.DB().
		Where(&database.Release{BizID: act.req.BizId, MultiReleaseID: multiReleaseID}).
		Find(&sts).Error

	if err != nil {
		return nil, nil, err
	}

	cfgIDs := []string{}
	releaseIDs := []string{}

	for _, st := range sts {
		cfgIDs = append(cfgIDs, st.CfgID)
		releaseIDs = append(releaseIDs, st.ReleaseID)
	}

	if len(cfgIDs) != len(releaseIDs) {
		return nil, nil, errors.New("invalid cfg_ids and release_ids num in multi release reload report")
	}
	return cfgIDs, releaseIDs, nil
}

func (act *CreateReleaseAction) createAppInstanceReleaseReload(info *pbcommon.ReportInfo) (pbcommon.ErrCode, string) {
	reloadTime, err := time.ParseInLocation("2006-01-02 15:04:05", info.ReloadTime, time.Local)
	if err != nil {
		logger.Warn("CreateAppInstanceRelease[%s]| invalid ReloadTime format, %+v", act.req.Seq, err)
		act.collector.StatAppInstanceRelease(false)
		return pbcommon.ErrCode_E_DM_SYSTEM_UNKNOWN, "invalid ReloadTime format"
	}

	finalCfgIDs := []string{}
	finalReleaseIDs := []string{}

	if len(info.ReleaseId) != 0 {
		cfgID, err := act.queryCfgidByReleae(info.ReleaseId)
		if err != nil {
			act.collector.StatAppInstanceRelease(false)
			return pbcommon.ErrCode_E_DM_SYSTEM_UNKNOWN, err.Error()
		}
		finalCfgIDs = append(finalCfgIDs, cfgID)
		finalReleaseIDs = append(finalReleaseIDs, info.ReleaseId)

	} else if len(info.MultiReleaseId) != 0 {
		cfgIDs, releaseIDs, err := act.queryIDsByMultiReleae(info.MultiReleaseId)
		if err != nil {
			act.collector.StatAppInstanceRelease(false)
			return pbcommon.ErrCode_E_DM_SYSTEM_UNKNOWN, err.Error()
		}
		finalCfgIDs = append(finalCfgIDs, cfgIDs...)
		finalReleaseIDs = append(finalReleaseIDs, releaseIDs...)

	} else {
		act.collector.StatAppInstanceRelease(false)
		return pbcommon.ErrCode_E_DM_SYSTEM_UNKNOWN, "invalid report type, releaseid and multiReleaseid all missing"
	}

	for i, cfgID := range finalCfgIDs {
		appInstanceRelease := database.AppInstanceRelease{
			InstanceID: act.appInstance.ID,
			BizID:      act.req.BizId,
			AppID:      act.req.AppId,
			CfgID:      cfgID,
			ReleaseID:  finalReleaseIDs[i],
			ReloadTime: &reloadTime,
			ReloadCode: info.ReloadCode,
			ReloadMsg:  info.ReloadMsg,
		}
		if len(info.ReloadMsg) > database.BSCPEFFECTRELOADERRLENLIMIT {
			// maybe a large error message.
			appInstanceRelease.ReloadMsg = info.ReloadMsg[:database.BSCPEFFECTRELOADERRLENLIMIT]
		}

		err := act.sd.DB().
			Where(database.AppInstanceRelease{
				InstanceID: act.appInstance.ID,
				CfgID:      cfgID,
				ReleaseID:  finalReleaseIDs[i],
			}).
			Assign(appInstanceRelease).
			FirstOrCreate(&appInstanceRelease).Error

		if err != nil {
			e, ok := err.(*mysql.MySQLError)
			if !ok {
				logger.Warn("CreateAppInstanceRelease[%s]| create instance release reload record, %+v",
					act.req.Seq, err)
				act.collector.StatAppInstanceRelease(false)
				continue
			}

			if e.Number != 1062 {
				logger.Warn("CreateAppInstanceRelease[%s]| create instance release reload record, %+v",
					act.req.Seq, err)
				act.collector.StatAppInstanceRelease(false)
				continue
			}

			tryErr := act.sd.DB().
				Where(database.AppInstanceRelease{
					InstanceID: act.appInstance.ID,
					CfgID:      cfgID,
					ReleaseID:  finalReleaseIDs[i],
				}).
				Assign(appInstanceRelease).
				FirstOrCreate(&appInstanceRelease).Error

			if tryErr != nil {
				logger.Warn("CreateAppInstanceRelease[%s]| create instance release reload record, try again, %+v",
					act.req.Seq, tryErr)
				act.collector.StatAppInstanceRelease(false)
				continue
			}
		}
		logger.V(4).Infof("CreateAppInstanceRelease[%s]| create app instance release reload record success, %+v",
			act.req.Seq, appInstanceRelease)
		act.collector.StatAppInstanceRelease(true)
	}
	return pbcommon.ErrCode_E_OK, ""
}

func (act *CreateReleaseAction) createAppInstanceRelease() (pbcommon.ErrCode, string) {
	for _, info := range act.req.Infos {
		if len(info.CfgId) != 0 {
			// normal sidecar effect configs report.
			act.createAppInstanceReleaseEffect(info)
		} else {
			// instance api reload report.
			act.createAppInstanceReleaseReload(info)
		}
	}

	return pbcommon.ErrCode_E_OK, ""
}

// Do makes the workflows of this action base on input messages.
func (act *CreateReleaseAction) Do() error {
	// business sharding db.
	sd, err := act.smgr.ShardingDB(act.req.BizId)
	if err != nil {
		return act.Err(pbcommon.ErrCode_E_DM_ERR_DBSHARDING, err.Error())
	}
	act.sd = sd

	// query appinstance.
	if errCode, errMsg := act.queryAppInstance(); errCode != pbcommon.ErrCode_E_OK {
		return act.Err(errCode, errMsg)
	}

	// create/update appinstance release.
	if errCode, errMsg := act.createAppInstanceRelease(); errCode != pbcommon.ErrCode_E_OK {
		return act.Err(errCode, errMsg)
	}
	return nil
}
