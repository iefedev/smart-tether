package dev.smarttether

import android.app.ActivityManager
import android.app.ActivityManager.RunningAppProcessInfo
import android.app.ApplicationExitInfo
import android.content.Context

/**
 * How Android ended the app's previous process. Some OEM builds (HyperOS:
 * ScreenOffCPUCheckKill) stop the relay with the screen off even with battery
 * optimization off. A new process opens new sockets, so every PC UDP flow gets a
 * new public port and long-lived UDP sessions such as voice calls must reconnect.
 */
object LastExit {
    fun previous(ctx: Context): ApplicationExitInfo? = runCatching {
        ctx.getSystemService(ActivityManager::class.java)
            .getHistoricalProcessExitReasons(ctx.packageName, 0, 1).firstOrNull()
    }.getOrNull()

    /** True when Android ended the process while it was relaying, not the user or an update. */
    fun unexpected(e: ApplicationExitInfo): Boolean {
        // The foreground service keeps the process at this importance while it relays.
        if (e.importance > RunningAppProcessInfo.IMPORTANCE_FOREGROUND_SERVICE) return false
        return when (e.reason) {
            ApplicationExitInfo.REASON_USER_REQUESTED, ApplicationExitInfo.REASON_USER_STOPPED,
            ApplicationExitInfo.REASON_EXIT_SELF, ApplicationExitInfo.REASON_PACKAGE_UPDATED,
            ApplicationExitInfo.REASON_PACKAGE_STATE_CHANGE -> false
            else -> true
        }
    }

    /** A short name: the OEM's own word when it gives one (HyperOS: "ScreenOffCPUCheckKill"). */
    fun label(e: ApplicationExitInfo): String {
        val oem = e.description?.trim()?.substringBefore(' ').orEmpty()
        if (e.reason == ApplicationExitInfo.REASON_OTHER && oem.isNotEmpty()) return oem
        return when (e.reason) {
            ApplicationExitInfo.REASON_LOW_MEMORY -> "low memory"
            ApplicationExitInfo.REASON_CRASH -> "crash"
            ApplicationExitInfo.REASON_CRASH_NATIVE -> "native crash"
            ApplicationExitInfo.REASON_ANR -> "not responding"
            ApplicationExitInfo.REASON_SIGNALED -> "signal ${e.status}"
            ApplicationExitInfo.REASON_EXCESSIVE_RESOURCE_USAGE -> "excessive resource use"
            ApplicationExitInfo.REASON_FREEZER -> "frozen"
            ApplicationExitInfo.REASON_PERMISSION_CHANGE -> "permission changed"
            else -> "stopped by the system"
        }
    }

    fun describe(e: ApplicationExitInfo) =
        "reason=${e.reason} importance=${e.importance} at=${e.timestamp} ${e.description.orEmpty()}"
}
