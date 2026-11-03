package dev.smarttether

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.wifi.WifiNetworkSpecifier
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.os.ParcelFileDescriptor
import android.os.PowerManager
import android.util.Log
import dev.smarttether.core.mobile.Binder
import dev.smarttether.core.mobile.Mobile
import java.net.Inet4Address

/**
 * Keeps the link to the PC alive in the background. Internet sockets are bound
 * to cellular; the PC's access point is joined as a local-only network (it never
 * becomes the default and never carries internet). Go does all the I/O.
 */
class RelayService : Service() {
    private lateinit var cm: ConnectivityManager
    private val main = Handler(Looper.getMainLooper())
    @Volatile private var cellular: Network? = null
    private var cellularDns: List<String> = emptyList()
    private var running = false
    private var started = false
    private var wakeLock: PowerManager.WakeLock? = null
    private var wifiCallback: ConnectivityManager.NetworkCallback? = null
    private var relayToken = ""  // the token the running relay was started with
    private var apPassInUse = "" // the passphrase of the current access point request

    private val cellularCallback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            val changed = cellular != network
            cellular = network
            log("cellular available: $network")
            // UDP sockets stay bound to the network they were opened on.
            if (changed) Mobile.cellularChanged()
        }

        override fun onLinkPropertiesChanged(network: Network, lp: LinkProperties) {
            // IPv4 resolvers first: the PC tunnel prefers IPv4.
            cellularDns = lp.dnsServers.sortedBy { if (it is Inet4Address) 0 else 1 }
                .mapNotNull { it.hostAddress }
            if (running) Mobile.setDNS(cellularDns.joinToString(",")) else maybeStartRelay()
        }

        override fun onLost(network: Network) {
            if (cellular == network) cellular = null
            log("cellular lost")
            Mobile.cellularChanged()
        }
    }

    private val binder = object : Binder {
        override fun bindCellular(fd: Long) = bind(cellular, fd)
        override fun log(msg: String) = this@RelayService.log(msg)
    }

    private fun bind(network: Network?, fd: Long): Boolean {
        network ?: return false
        return try {
            // fromFd dups the descriptor; binding the dup binds the same socket.
            ParcelFileDescriptor.fromFd(fd.toInt()).use { network.bindSocket(it.fileDescriptor) }
            true
        } catch (e: Exception) {
            log("bindSocket failed: $e")
            false
        }
    }

    override fun onCreate() {
        super.onCreate()
        cm = getSystemService(ConnectivityManager::class.java)
        LogFile.open(this)
        reportLastExit()
    }

    /** Logs how the previous process ended and, if Android killed the relay, tells the PC. */
    private fun reportLastExit() {
        val e = LastExit.previous(this) ?: return
        log("previous process ended: ${LastExit.describe(e)}")
        if (LastExit.unexpected(e)) Mobile.setLastExit(e.timestamp, LastExit.label(e), e.description.orEmpty())
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopSelf()
            return START_NOT_STICKY
        }
        startForeground(NOTIFICATION_ID, notification("Starting"), ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE)
        Mobile.setDeviceName(deviceName())
        // Compare with what is in use, not with the prefs before this call: the
        // QR scan and the PC's USB pairing write the prefs before starting us.
        if (started && running && Prefs.token(this) != relayToken) restartRelay()
        if (started && Prefs.apPass(this) != apPassInUse) {
            wifiCallback?.let { runCatching { cm.unregisterNetworkCallback(it) } }
            requestAccessPoint()
        }
        if (!started) {
            started = true
            isRunning = true
            wakeLock = getSystemService(PowerManager::class.java)
                .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "SmartTether:relay").apply { acquire() }
            cm.requestNetwork(
                NetworkRequest.Builder()
                    .addTransportType(NetworkCapabilities.TRANSPORT_CELLULAR)
                    .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                    .build(), cellularCallback
            )
            requestAccessPoint()
        }
        maybeStartRelay()
        return START_STICKY
    }

    /** Asks Android to join the PC's access point as a local-only network. */
    private fun requestAccessPoint() {
        val ssid = Prefs.apSsid(this)
        val pass = Prefs.apPass(this)
        apPassInUse = pass
        if (pass.isEmpty()) {
            log("no access point passphrase configured")
            return
        }
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onLinkPropertiesChanged(network: Network, lp: LinkProperties) {
                // The PC dials us here; we never listen on the cellular interface.
                val ip = lp.linkAddresses.map { it.address }.firstOrNull { it is Inet4Address }?.hostAddress ?: ""
                log("access point address: $ip")
                Mobile.setWifiAddress(ip)
            }

            override fun onLost(network: Network) {
                log("access point lost")
                Mobile.setWifiAddress("")
                retryAccessPoint()
            }

            override fun onUnavailable() {
                log("access point unavailable")
                retryAccessPoint()
            }
        }
        wifiCallback = cb
        val spec = WifiNetworkSpecifier.Builder().setSsid(ssid).setWpa2Passphrase(pass).build()
        cm.requestNetwork(
            NetworkRequest.Builder()
                .addTransportType(NetworkCapabilities.TRANSPORT_WIFI)
                .removeCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                .setNetworkSpecifier(spec)
                .build(), cb
        )
        log("requested access point $ssid")
    }

    private fun retryAccessPoint() {
        main.postDelayed({
            if (!started) return@postDelayed
            wifiCallback?.let { runCatching { cm.unregisterNetworkCallback(it) } }
            requestAccessPoint()
        }, 5000)
    }

    /** A new pairing arrived (QR or USB): start over with the new token. */
    @Synchronized
    private fun restartRelay() {
        Mobile.stop()
        running = false
        log("pairing changed, restarting relay")
        maybeStartRelay()
    }

    /** The name the user gave the phone (Settings › About), shown on the PC. */
    private fun deviceName(): String =
        android.provider.Settings.Global.getString(contentResolver, android.provider.Settings.Global.DEVICE_NAME)
            ?.takeIf { it.isNotBlank() } ?: android.os.Build.MODEL

    @Synchronized
    private fun maybeStartRelay() {
        if (running || cellular == null || cellularDns.isEmpty()) return
        val token = Prefs.token(this)
        if (token.isEmpty()) {
            log("no token configured")
            return
        }
        try {
            Mobile.start(token, cellularDns.joinToString(","), binder)
            running = true
            relayToken = token
            log("relay started, dns=$cellularDns")
            getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, notification("Running"))
        } catch (e: Exception) {
            log("relay start failed: $e")
        }
    }

    override fun onDestroy() {
        started = false
        isRunning = false
        main.removeCallbacksAndMessages(null)
        Mobile.stop()
        running = false
        runCatching { cm.unregisterNetworkCallback(cellularCallback) }
        wifiCallback?.let { runCatching { cm.unregisterNetworkCallback(it) } }
        wakeLock?.release()
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun notification(text: String): Notification {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL, "Tether", NotificationManager.IMPORTANCE_LOW))
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        return Notification.Builder(this, CHANNEL)
            .setContentTitle("Smart Tether")
            .setContentText(text)
            .setSmallIcon(R.drawable.ic_stat_tether)
            .setContentIntent(open)
            .setOngoing(true)
            .build()
    }

    private fun log(msg: String) {
        Log.i(TAG, msg)
        LogFile.write(msg)
    }

    companion object {
        const val TAG = "SmartTether"
        const val ACTION_STOP = "dev.smarttether.STOP"
        private const val CHANNEL = "relay"
        private const val NOTIFICATION_ID = 1

        /** True while the service runs (read by the UI in the same process). */
        @Volatile var isRunning = false
            private set

        fun start(ctx: Context) = ctx.startForegroundService(Intent(ctx, RelayService::class.java))
        fun stop(ctx: Context) = ctx.startService(Intent(ctx, RelayService::class.java).setAction(ACTION_STOP))
    }
}
