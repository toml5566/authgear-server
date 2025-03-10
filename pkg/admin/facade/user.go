package facade

import (
	"time"

	"github.com/authgear/authgear-server/pkg/admin/model"
	"github.com/authgear/authgear-server/pkg/api"
	apimodel "github.com/authgear/authgear-server/pkg/api/model"
	"github.com/authgear/authgear-server/pkg/lib/authn/authenticator"
	"github.com/authgear/authgear-server/pkg/lib/authn/authenticator/service"
	"github.com/authgear/authgear-server/pkg/lib/authn/identity"
	"github.com/authgear/authgear-server/pkg/lib/authn/user"
	"github.com/authgear/authgear-server/pkg/lib/config"
	libes "github.com/authgear/authgear-server/pkg/lib/elasticsearch"
	interactionintents "github.com/authgear/authgear-server/pkg/lib/interaction/intents"
	"github.com/authgear/authgear-server/pkg/util/clock"
	"github.com/authgear/authgear-server/pkg/util/graphqlutil"
)

type UserService interface {
	CreateByAdmin(identitySpec *identity.Spec, password string, generatePassword bool, sendPassword bool, setPasswordExpired bool) (*user.User, error)
	GetRaw(id string) (*user.User, error)
	Count() (uint64, error)
	QueryPage(listOption user.ListOptions, pageArgs graphqlutil.PageArgs) ([]apimodel.PageItemRef, error)
	Delete(userID string) error
	Disable(userID string, reason *string) error
	Reenable(userID string) error
	ScheduleDeletionByAdmin(userID string) error
	UnscheduleDeletionByAdmin(userID string) error
	Anonymize(userID string) error
	ScheduleAnonymizationByAdmin(userID string) error
	UnscheduleAnonymizationByAdmin(userID string) error
	CheckUserAnonymized(userID string) error
	UpdateMFAEnrollment(userID string, endAt *time.Time) error
}

type UserSearchService interface {
	QueryUser(searchKeyword string,
		filterOptions user.FilterOptions,
		sortOption user.SortOption,
		pageArgs graphqlutil.PageArgs) ([]apimodel.PageItemRef, *libes.Stats, error)
}

type UserFacade struct {
	Clock              clock.Clock
	UserSearchService  UserSearchService
	Users              UserService
	LoginIDConfig      *config.LoginIDConfig
	Authenticators     AuthenticatorService
	StandardAttributes StandardAttributesService
	Interaction        InteractionService
}

func (f *UserFacade) ListPage(listOption user.ListOptions, pageArgs graphqlutil.PageArgs) ([]apimodel.PageItemRef, *graphqlutil.PageResult, error) {
	values, err := f.Users.QueryPage(listOption, pageArgs)
	if err != nil {
		return nil, nil, err
	}

	return values, graphqlutil.NewPageResult(pageArgs, len(values), graphqlutil.NewLazy(func() (interface{}, error) {
		return f.Users.Count()
	})), nil
}

func (f *UserFacade) SearchPage(
	searchKeyword string,
	filterOptions user.FilterOptions,
	sortOption user.SortOption,
	pageArgs graphqlutil.PageArgs) ([]apimodel.PageItemRef, *graphqlutil.PageResult, error) {
	refs, stats, err := f.UserSearchService.QueryUser(searchKeyword, filterOptions, sortOption, pageArgs)
	if err != nil {
		return nil, nil, err
	}
	return refs, graphqlutil.NewPageResult(pageArgs, len(refs), graphqlutil.NewLazy(func() (interface{}, error) {
		return stats.TotalCount, nil
	})), nil
}

func (f *UserFacade) Create(identityDef model.IdentityDef, password string, generatePassword bool, sendPassword bool, setPasswordExpired bool) (userID string, err error) {
	// NOTE: identityDef is assumed to be a login ID since portal only supports login ID
	loginIDInput := identityDef.(*model.IdentityDefLoginID)
	loginIDKeyCofig, ok := f.LoginIDConfig.GetKeyConfig(loginIDInput.Key)
	if !ok {
		return "", api.NewInvariantViolated("InvalidLoginIDKey", "invalid login ID key", nil)
	}

	identitySpec := &identity.Spec{
		Type: identityDef.Type(),
		LoginID: &identity.LoginIDSpec{
			Key:   loginIDInput.Key,
			Type:  loginIDKeyCofig.Type,
			Value: loginIDInput.Value,
		},
	}

	user, err := f.Users.CreateByAdmin(
		identitySpec,
		password,
		generatePassword,
		sendPassword,
		setPasswordExpired,
	)
	if err != nil {
		return "", err
	}

	return user.ID, nil
}

func (f *UserFacade) ResetPassword(id string, password string, generatePassword bool, sendPassword bool, changeOnLogin bool) (err error) {
	err = f.Users.CheckUserAnonymized(id)
	if err != nil {
		return err
	}

	_, err = f.Interaction.Perform(
		interactionintents.NewIntentResetPassword(),
		&resetPasswordInput{userID: id, password: password, generatePassword: generatePassword, sendPassword: sendPassword, changeOnLogin: changeOnLogin},
	)
	if err != nil {
		return err
	}
	return nil
}

func (f *UserFacade) SetPasswordExpired(id string, isExpired bool) error {
	err := f.Users.CheckUserAnonymized(id)
	if err != nil {
		return err
	}

	passwordType := apimodel.AuthenticatorTypePassword
	primaryKind := authenticator.KindPrimary
	ars, err := f.Authenticators.ListRefsByUsers(
		[]string{id},
		&passwordType,
		&primaryKind,
	)
	if err != nil {
		return err
	}

	if len(ars) == 0 {
		return api.ErrAuthenticatorNotFound
	}

	for _, ai := range ars {
		a, err := f.Authenticators.Get(ai.ID)
		if err != nil {
			return err
		}

		if a.Password == nil {
			continue
		}

		var expireAfter *time.Time
		if isExpired {
			now := f.Clock.NowUTC()
			expireAfter = &now
		}

		_, a, err = f.Authenticators.UpdatePassword(a, &service.UpdatePasswordOptions{
			SetExpireAfter: true,
			ExpireAfter:    expireAfter,
		})
		if err != nil {
			return err
		}

		err = f.Authenticators.Update(a)
		if err != nil {
			return err
		}
	}

	return nil
}

func (f *UserFacade) SetDisabled(id string, isDisabled bool, reason *string) error {
	var err error
	if isDisabled {
		err = f.Users.Disable(id, reason)
	} else {
		err = f.Users.Reenable(id)
	}
	if err != nil {
		return err
	}
	return nil
}

func (f *UserFacade) ScheduleDeletion(id string) error {
	err := f.Users.ScheduleDeletionByAdmin(id)
	if err != nil {
		return err
	}
	return nil
}

func (f *UserFacade) UnscheduleDeletion(id string) error {
	err := f.Users.UnscheduleDeletionByAdmin(id)
	if err != nil {
		return err
	}
	return nil
}

func (f *UserFacade) Delete(id string) error {
	err := f.Users.Delete(id)
	if err != nil {
		return err
	}
	return nil
}

func (f *UserFacade) ScheduleAnonymization(id string) (err error) {
	err = f.Users.CheckUserAnonymized(id)
	if err != nil {
		return err
	}

	err = f.Users.ScheduleAnonymizationByAdmin(id)
	if err != nil {
		return err
	}
	return nil
}

func (f *UserFacade) UnscheduleAnonymization(id string) (err error) {
	err = f.Users.CheckUserAnonymized(id)
	if err != nil {
		return err
	}

	err = f.Users.UnscheduleAnonymizationByAdmin(id)
	if err != nil {
		return err
	}
	return nil
}

func (f *UserFacade) Anonymize(id string) (err error) {
	err = f.Users.CheckUserAnonymized(id)
	if err != nil {
		return err
	}

	err = f.Users.Anonymize(id)
	if err != nil {
		return err
	}
	return nil
}

func (f *UserFacade) SetMFAGracePeriod(id string, endAt *time.Time) error {
	err := f.Users.CheckUserAnonymized(id)
	if err != nil {
		return err
	}

	err = f.Users.UpdateMFAEnrollment(id, endAt)
	if err != nil {
		return err
	}

	return nil
}
