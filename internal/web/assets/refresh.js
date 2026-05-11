// Reload the status page every minute while it is visible. A hidden tab does
// not refresh, and comes back up to date as soon as it is shown again.
(function () {
  var every = 60000
  var last = Date.now()
  setInterval(function () {
    if (document.visibilityState === 'visible' && Date.now() - last >= every) location.reload()
  }, 5000)
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible' && Date.now() - last >= every) location.reload()
  })
})()
