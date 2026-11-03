package dev.smarttether

import android.content.Context
import java.io.File
import java.io.FileWriter
import java.io.Writer
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * Keeps the relay log in a file as well: logcat holds only minutes of it, too
 * little to see what happened overnight. Read it from the PC with
 * `adb pull /sdcard/Android/data/dev.smarttether/files/relay.log`.
 */
object LogFile {
    private const val MAX_BYTES = 4L shl 20 // then it moves to relay.log.old
    private val stamp = SimpleDateFormat("yyyy-MM-dd HH:mm:ss.SSS", Locale.US)
    private var file: File? = null
    private var out: Writer? = null
    private var size = 0L

    @Synchronized
    fun open(ctx: Context) {
        if (file != null) return
        file = File(ctx.getExternalFilesDir(null) ?: ctx.filesDir, "relay.log")
        reopen()
    }

    @Synchronized
    fun write(msg: String) {
        val w = out ?: return
        val line = "${stamp.format(Date())} $msg\n"
        // Flushed per line: the process can be killed at any moment.
        runCatching { w.write(line); w.flush() }
        size += line.length
        if (size > MAX_BYTES) {
            runCatching { w.close() }
            val f = file ?: return
            f.renameTo(File(f.path + ".old"))
            reopen()
        }
    }

    private fun reopen() {
        val f = file ?: return
        out = runCatching { FileWriter(f, true).buffered() }.getOrNull()
        size = f.length()
    }
}
