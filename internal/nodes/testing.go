package nodes

// AllowInsecureAPIHosts lets tests of other packages send access tokens to
// plain-http test servers on apiHosts. Never call it outside tests.
func AllowInsecureAPIHosts() { allowInsecureAPIHosts = true }
