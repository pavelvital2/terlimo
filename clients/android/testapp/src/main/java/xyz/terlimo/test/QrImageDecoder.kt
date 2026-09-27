package xyz.terlimo.test

import android.content.ContentResolver
import android.graphics.BitmapFactory
import android.net.Uri
import com.google.zxing.BinaryBitmap
import com.google.zxing.MultiFormatReader
import com.google.zxing.RGBLuminanceSource
import com.google.zxing.common.HybridBinarizer

object QrImageDecoder {
    private const val MAX_SIDE = 2048
    private const val MAX_PIXELS = 32_000_000L

    fun decode(resolver: ContentResolver, uri: Uri): String {
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        resolver.openInputStream(uri).use { input ->
            require(input != null) { "QR_IMAGE_INVALID" }
            BitmapFactory.decodeStream(input, null, bounds)
        }
        require(bounds.outWidth > 0 && bounds.outHeight > 0 &&
            bounds.outWidth.toLong() * bounds.outHeight <= MAX_PIXELS) { "QR_IMAGE_INVALID" }
        var sample = 1
        while (bounds.outWidth / sample > MAX_SIDE || bounds.outHeight / sample > MAX_SIDE) sample *= 2
        val bitmap = resolver.openInputStream(uri).use { input ->
            requireNotNull(BitmapFactory.decodeStream(input, null,
                BitmapFactory.Options().apply { inSampleSize = sample })) { "QR_IMAGE_INVALID" }
        }
        val pixels = IntArray(bitmap.width * bitmap.height)
        bitmap.getPixels(pixels, 0, bitmap.width, 0, 0, bitmap.width, bitmap.height)
        val source = RGBLuminanceSource(bitmap.width, bitmap.height, pixels)
        return MultiFormatReader().decode(BinaryBitmap(HybridBinarizer(source))).text
    }
}
