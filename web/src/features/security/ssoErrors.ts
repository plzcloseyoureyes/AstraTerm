/** Human messages for the ?sso_error= codes the SSO callback redirects with (SPEC §9 "security"). */
const MESSAGES: Record<string, string> = {
  unknown_provider: 'This sign-in option is no longer available.',
  provider_unreachable: 'The identity provider could not be reached. Try again in a moment.',
  provider_misconfigured: 'Single sign-on is misconfigured. Ask your administrator.',
  busy: 'Too many sign-ins in progress. Try again in a moment.',
  invalid_state: 'The sign-in link expired or was opened in another browser. Start again from this page.',
  expired: 'The sign-in took too long. Please try again.',
  access_denied: 'Sign-in was cancelled at the identity provider.',
  idp_error: 'The identity provider reported an error.',
  verification_failed: 'The identity provider’s answer could not be verified.',
  email_not_verified: 'Your e-mail address is not verified at the identity provider.',
  not_allowed: 'Your account is not in a group allowed to use Termstead.',
  not_provisioned: 'There is no Termstead account for you yet. Ask your administrator to create one.',
  provisioning_failed: 'Your Termstead account could not be created. Ask your administrator.',
  already_linked: 'That identity is already linked to another Termstead account.',
  not_signed_in: 'Sign in first to link an account.',
  account_disabled: 'Your Termstead account is disabled.',
  login_not_allowed: 'Sign-in is not allowed from your network.',
  mfa_required: 'This account must sign in with a second factor.',
  reauth_required: 'Confirm it’s you first: linking an account needs a recent sign-in.',
}

export function ssoErrorMessage(code: string): string {
  return MESSAGES[code] ?? `Single sign-on failed (${code}).`
}
